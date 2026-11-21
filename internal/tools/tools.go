package tools

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

// ErrWriteConfirmationRequired is returned by Execute when a tool
// produces a file mutation that needs the user's explicit approval.
var ErrWriteConfirmationRequired = fmt.Errorf("write_file: user confirmation required")

// PendingWrite holds the details of a proposed file write awaiting user confirmation.
type PendingWrite struct {
	Path    string
	Content string
	// Diff is a human-readable unified diff shown in the confirmation UI.
	// Empty for new files (no previous content to diff against).
	Diff string

	// Original is the file content the diff was computed against, and Existed
	// whether the file existed then. CommitWrite refuses to write if the file
	// changed in the meantime, so a stale proposal can't clobber newer edits.
	Original string
	Existed  bool

	// Notes describe fuzzy matching that was applied (shown to the user and
	// reported back to the model).
	Notes []string
}

// ErrStaleWrite is returned by CommitWrite when the target changed after the
// write was proposed.
var ErrStaleWrite = fmt.Errorf("file changed on disk since this write was proposed; re-read it and try again")

// CommitWrite flushes a confirmed PendingWrite to disk.
func CommitWrite(pw PendingWrite) error {
	current, err := os.ReadFile(pw.Path)
	switch {
	case err == nil:
		if !pw.Existed || string(current) != pw.Original {
			return fmt.Errorf("write %s: %w", pw.Path, ErrStaleWrite)
		}
	case os.IsNotExist(err):
		if pw.Existed {
			return fmt.Errorf("write %s: %w", pw.Path, ErrStaleWrite)
		}
	default:
		return fmt.Errorf("write_file: cannot verify %s: %w", pw.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(pw.Path), 0o750); err != nil {
		return fmt.Errorf("write_file: cannot create parent directory: %w", err)
	}
	if err := os.WriteFile(pw.Path, []byte(pw.Content), 0o644); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	return nil
}

// Definitions returns the native tools offered to the LLM.
func Definitions() []llm.Tool {
	return []llm.Tool{
		// ── read_file ────────────────────────────────────────────────────────
		{
			Name:        "read_file",
			Description: "Read a text file. Each output line is prefixed with its line number and a TAB (\"    42\\tcode\"); these prefixes are NOT part of the file — never include them in edit_file old_content. Large files are cut off; use start_line/end_line to read a specific range.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The path to the file to read",
					},
					"start_line": map[string]any{
						"type":        "integer",
						"description": "First line to read, 1-indexed inclusive. Omit to start from the beginning.",
					},
					"end_line": map[string]any{
						"type":        "integer",
						"description": "Last line to read, 1-indexed inclusive. Omit or set to 0 to read to end of file.",
					},
				},
				"required": []string{"path"},
			},
		},
		// ── write_file ───────────────────────────────────────────────────────
		{
			Name:        "write_file",
			Description: "Write (or overwrite) a file with the given content. The user will be prompted to confirm before the write is committed. Prefer edit_file when modifying an existing file.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The file path to write to (relative or absolute)",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "The full content to write to the file",
					},
				},
				"required": []string{"path", "content"},
			},
		},
		// ── edit_file ────────────────────────────────────────────────────────
		{
			Name: "edit_file",
			Description: `Replace an exact snippet of text inside an existing file with new content.
'old_content' must match exactly once in the file — include enough surrounding lines to make it unique.
The user will see a diff preview before the change is committed.
Always prefer this over write_file when editing an existing file.`,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to edit",
					},
					"old_content": map[string]any{
						"type":        "string",
						"description": "The exact text to find and replace. Must appear exactly once in the file.",
					},
					"new_content": map[string]any{
						"type":        "string",
						"description": "The replacement text",
					},
					"replace_all": map[string]any{
						"type":        "boolean",
						"description": "Replace every occurrence instead of requiring a unique match (e.g. renaming a variable within the file).",
					},
				},
				"required": []string{"path", "old_content", "new_content"},
			},
		},
		// ── multi_edit ───────────────────────────────────────────────────────
		{
			Name: "multi_edit",
			Description: `Apply several search/replace edits to ONE file in a single step (one diff, one confirmation).
Edits are applied in order; each sees the result of the previous one. Each old_content must match exactly once unless replace_all is set.
If any edit fails, nothing is written. Prefer this over several edit_file calls on the same file.`,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Path to the file to edit",
					},
					"edits": map[string]any{
						"type":        "array",
						"description": "The edits to apply, in order",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"old_content": map[string]any{"type": "string", "description": "Exact text to replace"},
								"new_content": map[string]any{"type": "string", "description": "Replacement text"},
								"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence"},
							},
							"required": []string{"old_content", "new_content"},
						},
					},
				},
				"required": []string{"path", "edits"},
			},
		},
		// ── find_files ───────────────────────────────────────────────────────
		{
			Name:        "find_files",
			Description: "Find files by glob pattern, respecting .gitignore. Patterns without '/' match file names anywhere (\"*.go\"); use '**' for any depth (\"internal/**/*_test.go\") and {a,b} for alternatives (\"*.{ts,tsx}\").",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Glob pattern",
					},
					"dir": map[string]any{
						"type":        "string",
						"description": "Directory to search (default: .)",
					},
				},
				"required": []string{"pattern"},
			},
		},
		// ── run_command ──────────────────────────────────────────────────────
		{
			Name: "run_command",
			Description: `Execute a shell command and return its stdout, stderr, and exit code.
Use for: building, testing, linting, running scripts, git operations, checking tool versions.
The user will be prompted to approve unless auto-approve mode is active (/approve-tools on).
Prefer specific, targeted commands. Avoid commands that start interactive sessions.`,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The shell command to execute",
					},
					"working_dir": map[string]any{
						"type":        "string",
						"description": "Working directory for the command. Defaults to the current directory.",
					},
					"timeout_seconds": map[string]any{
						"type":        "integer",
						"description": "Max seconds to wait (1-120, default 30)",
					},
				},
				"required": []string{"command"},
			},
		},
		// ── list_directory ───────────────────────────────────────────────────
		{
			Name:        "list_directory",
			Description: "List the files and directories within a given directory (non-recursive, like ls).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "The directory path to list (default: .)",
					},
				},
			},
		},
		// ── search_regex ─────────────────────────────────────────────────────
		{
			Name:        "search_regex",
			Description: "Search file contents for a regular expression (like grep -rn), respecting .gitignore and skipping binary files. Returns path:line: text for up to 50 matches.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "The regular expression pattern to search for",
					},
					"dir": map[string]any{
						"type":        "string",
						"description": "The directory to search in (default: .)",
					},
					"file_glob": map[string]any{
						"type":        "string",
						"description": "Optional glob to filter files (e.g. *.go, src/**/*.ts).",
					},
					"case_insensitive": map[string]any{
						"type":        "boolean",
						"description": "Match case-insensitively",
					},
				},
				"required": []string{"pattern"},
			},
		},
		// ── map_workspace ────────────────────────────────────────────────────
		{
			Name:        "map_workspace",
			Description: "Returns a tree-like map of a workspace directory. Use this to orient yourself in an unfamiliar codebase.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir": map[string]any{
						"type":        "string",
						"description": "The directory to map (default: .)",
					},
				},
			},
		},
		// ── repo_map ─────────────────────────────────────────────────────────
		{
			Name: "repo_map",
			Description: "Map a codebase's definitions: functions, types, classes and their signatures with line numbers, grouped by file and ranked by how often they are referenced, cut to a token budget. " +
				"Use it to find the important code and the right file before reading. Go is parsed exactly; Python, JS/TS, Rust and shell by line patterns. " +
				"Pass focus to boost files or symbols you care about.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir": map[string]any{
						"type":        "string",
						"description": "Directory to map (default: .). Narrow it for big repos.",
					},
					"focus": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "File paths (or path fragments like 'internal/agent') and symbol names to rank first.",
					},
					"max_tokens": map[string]any{
						"type":        "integer",
						"description": "Output budget in tokens (default 4096, max 16384).",
					},
				},
			},
		},
		// ── explore ──────────────────────────────────────────────────────────
		{
			Name: "explore",
			Description: "Delegate a codebase question to a subagent that has a fresh context and local read-only tools (repo_map, search_regex, find_files, read_file, …). It investigates and returns its findings: direct answer, path:line evidence, how the pieces connect. " +
				"Use it for broad questions that would take many searches and reads (\"where is X handled and how does it reach Y?\", \"what calls this and with which arguments?\"), so the raw output doesn't fill your context. " +
				"Don't use it for a single lookup that one search_regex or read_file answers. You don't see its tool output: read the files yourself before editing them. Several independent questions can be separate explore calls.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"question": map[string]any{
						"type":        "string",
						"description": "A specific, self-contained question. Say what you need back (e.g. \"list each call site with path:line\").",
					},
					"hints": map[string]any{
						"type":        "string",
						"description": "Optional starting points: files, packages or symbol names you already know are relevant.",
					},
				},
				"required": []string{"question"},
			},
		},
		// ── web_search ───────────────────────────────────────────────────────
		{
			Name:        "web_search",
			Description: "Search the web for real-time information. Returns titles, snippets, and URLs. Follow up with web_fetch to read a full page.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "The search query",
					},
				},
				"required": []string{"query"},
			},
		},
		// ── web_fetch ────────────────────────────────────────────────────────
		{
			Name:        "web_fetch",
			Description: "Fetch and extract the text content of a specific URL. Strips navigation, ads, and scripts. Code blocks are preserved. Use after web_search to read documentation or articles.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "The full URL to fetch",
					},
				},
				"required": []string{"url"},
			},
		},
	}
}

// ── Tool execution ────────────────────────────────────────────────────────────

// Execute executes a tool call and returns its string result.
// For file-mutation tools (write_file, edit_file) it returns a PendingWrite
// that the TUI must confirm before calling CommitWrite.
// For run_command it returns ErrWriteConfirmationRequired so the TUI can gate
// execution through its approval flow; the sandbox.Options are embedded in the
// returned PendingWrite with Path="" to signal "command, not a file".
func Execute(call llm.ToolCall) (result string, pw *PendingWrite, err error) {
	switch call.Name {

	// ── read_file ────────────────────────────────────────────────────────────
	case "read_file":
		path, ok := call.Args["path"].(string)
		if !ok {
			return "", nil, fmt.Errorf("read_file: missing or invalid 'path' argument")
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}
		if looksBinary(data) {
			return fmt.Sprintf("%s appears to be a binary file (%d bytes); not shown.", path, len(data)), nil, nil
		}
		startLine, _ := intArg(call.Args, "start_line")
		endLine, _ := intArg(call.Args, "end_line")
		return numberedLines(path, string(data), startLine, endLine), nil, nil

	// ── write_file ───────────────────────────────────────────────────────────
	case "write_file":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			return "", nil, fmt.Errorf("write_file: missing 'path' argument")
		}
		content, ok := call.Args["content"].(string)
		if !ok {
			return "", nil, fmt.Errorf("write_file: missing 'content' argument")
		}
		existing, readErr := os.ReadFile(path)
		diff := simpleDiff(string(existing), content)
		return "", &PendingWrite{Path: path, Content: content, Diff: diff, Original: string(existing), Existed: readErr == nil}, ErrWriteConfirmationRequired

	// ── edit_file ────────────────────────────────────────────────────────────
	case "edit_file":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			return "", nil, fmt.Errorf("edit_file: missing 'path' argument")
		}
		oldContent, ok := call.Args["old_content"].(string)
		if !ok {
			return "", nil, fmt.Errorf("edit_file: missing 'old_content' argument")
		}
		newContent, ok := call.Args["new_content"].(string)
		if !ok {
			return "", nil, fmt.Errorf("edit_file: missing 'new_content' argument")
		}

		replaceAll, _ := call.Args["replace_all"].(bool)

		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("edit_file: cannot read %s: %w", path, err)
		}
		fileStr := string(data)
		newFileStr, note, err := applyEdit(fileStr, editOp{Old: oldContent, New: newContent, ReplaceAll: replaceAll})
		if err != nil {
			return "", nil, fmt.Errorf("edit_file %s: %w", path, err)
		}
		pw := &PendingWrite{Path: path, Content: newFileStr, Diff: simpleDiff(fileStr, newFileStr), Original: fileStr, Existed: true}
		if note != "" {
			pw.Notes = append(pw.Notes, note)
		}
		return "", pw, ErrWriteConfirmationRequired

	// ── multi_edit ───────────────────────────────────────────────────────────
	case "multi_edit":
		path, ok := call.Args["path"].(string)
		if !ok || path == "" {
			return "", nil, fmt.Errorf("multi_edit: missing 'path' argument")
		}
		ops, err := parseEditOps(call.Args["edits"])
		if err != nil {
			return "", nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", nil, fmt.Errorf("multi_edit: cannot read %s: %w", path, err)
		}
		original := string(data)
		current := original
		var notes []string
		for i, op := range ops {
			next, note, err := applyEdit(current, op)
			if err != nil {
				return "", nil, fmt.Errorf("multi_edit %s: edit #%d: %w (no edits were applied)", path, i+1, err)
			}
			if note != "" {
				notes = append(notes, fmt.Sprintf("edit #%d: %s", i+1, note))
			}
			current = next
		}
		return "", &PendingWrite{Path: path, Content: current, Diff: simpleDiff(original, current), Original: original, Existed: true, Notes: notes}, ErrWriteConfirmationRequired

	// ── run_command ──────────────────────────────────────────────────────────
	// run_command is gated by the TUI (auto-approve or confirmation dialog).
	// We return ErrWriteConfirmationRequired with a sentinel PendingWrite
	// (Path == "") so the TUI knows to route it through the command approval flow.
	case "run_command":
		command, ok := call.Args["command"].(string)
		if !ok || command == "" {
			return "", nil, fmt.Errorf("run_command: missing 'command' argument")
		}
		return "", &PendingWrite{Path: "", Content: command}, ErrWriteConfirmationRequired

	// ── list_directory ───────────────────────────────────────────────────────
	case "list_directory":
		dir, _ := call.Args["path"].(string)
		if dir == "" {
			dir = "."
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", nil, fmt.Errorf("list_directory: %w", err)
		}
		var lines []string
		for _, e := range entries {
			info, _ := e.Info()
			var size string
			if !e.IsDir() && info != nil {
				size = fmt.Sprintf(" (%d B)", info.Size())
			}
			suffix := ""
			if e.IsDir() {
				suffix = "/"
			}
			lines = append(lines, fmt.Sprintf("%s%s%s", e.Name(), suffix, size))
		}
		if len(lines) == 0 {
			return "(empty directory)", nil, nil
		}
		return strings.Join(lines, "\n"), nil, nil

	// ── search_regex ─────────────────────────────────────────────────────────
	case "search_regex":
		pattern, ok := call.Args["pattern"].(string)
		if !ok || pattern == "" {
			return "", nil, fmt.Errorf("search_regex: missing 'pattern' argument")
		}
		dir, _ := call.Args["dir"].(string)
		if dir == "" {
			dir = "."
		}
		fileGlob, _ := call.Args["file_glob"].(string)

		caseInsensitive, _ := call.Args["case_insensitive"].(bool)
		out, err := searchFiles(pattern, dir, fileGlob, caseInsensitive)
		if err != nil {
			return "", nil, err
		}
		return out, nil, nil

	// ── find_files ───────────────────────────────────────────────────────────
	case "find_files":
		pattern, ok := call.Args["pattern"].(string)
		if !ok || pattern == "" {
			return "", nil, fmt.Errorf("find_files: missing 'pattern' argument")
		}
		dir, _ := call.Args["dir"].(string)
		if dir == "" {
			dir = "."
		}
		out, err := findFiles(pattern, dir)
		if err != nil {
			return "", nil, err
		}
		return out, nil, nil

	// ── map_workspace ────────────────────────────────────────────────────────
	case "map_workspace":
		dir, ok := call.Args["dir"].(string)
		if !ok || dir == "" {
			dir = "."
		}
		tree, err := workspace.MapDirectory(dir)
		if err != nil {
			return "", nil, err
		}
		return tree, nil, nil

	// ── explore ──────────────────────────────────────────────────────────────
	case "explore":
		return "", nil, fmt.Errorf("explore is run by the agent, not tools.Execute")

	// ── repo_map ─────────────────────────────────────────────────────────────
	case "repo_map":
		return repoMap(call)

	// ── web_search ───────────────────────────────────────────────────────────
	case "web_search":
		query, ok := call.Args["query"].(string)
		if !ok || query == "" {
			return "", nil, fmt.Errorf("web_search: missing query parameter")
		}

		client := &http.Client{Timeout: 10 * time.Second}
		req, err := http.NewRequest("POST", "https://lite.duckduckgo.com/lite/",
			strings.NewReader("q="+url.QueryEscape(query)))
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")

		resp, err := client.Do(req)
		if err != nil {
			return "", nil, err
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
		if err != nil {
			return "", nil, err
		}
		htmlBody := string(body)

		titles := reDDGTitle.FindAllStringSubmatch(htmlBody, 8)
		urls := reDDGURL.FindAllStringSubmatch(htmlBody, 8)
		snippets := reDDGSnippet.FindAllStringSubmatch(htmlBody, 8)

		if len(snippets) == 0 && len(titles) == 0 {
			return "No results found for: " + query, nil, nil
		}

		stripTags := reTag
		var results []string
		n := len(snippets)
		if len(titles) > n {
			n = len(titles)
		}
		for i := 0; i < n && i < 6; i++ {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("%d. ", i+1))

			if i < len(titles) {
				title := strings.TrimSpace(html.UnescapeString(stripTags.ReplaceAllString(titles[i][1], "")))
				if title != "" {
					sb.WriteString("**" + title + "**\n   ")
				}
			}
			if i < len(snippets) {
				snippet := strings.TrimSpace(html.UnescapeString(stripTags.ReplaceAllString(snippets[i][1], "")))
				sb.WriteString(snippet)
			}
			if i < len(urls) {
				u := strings.TrimSpace(urls[i][1])
				if u != "" {
					sb.WriteString("\n   " + u)
				}
			}
			results = append(results, sb.String())
		}
		return strings.Join(results, "\n\n"), nil, nil

	// ── web_fetch ────────────────────────────────────────────────────────────
	case "web_fetch", "web_scrape":
		targetURL, ok := call.Args["url"].(string)
		if !ok || targetURL == "" {
			return "", nil, fmt.Errorf("web_fetch: missing url parameter")
		}

		client := &http.Client{Timeout: 15 * time.Second}
		req, err := http.NewRequest("GET", targetURL, nil)
		if err != nil {
			return "", nil, err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")

		resp, err := client.Do(req)
		if err != nil {
			return "", nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			return "", nil, fmt.Errorf("web_fetch: HTTP %d for %s", resp.StatusCode, targetURL)
		}

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBodyBytes))
		if err != nil {
			return "", nil, err
		}
		htmlStr := strings.ToValidUTF8(string(bodyBytes), "")

		// Preserve code blocks before stripping tags.
		htmlStr = reCodeBlock.ReplaceAllStringFunc(htmlStr, func(m string) string {
			inner := reCodeBlock.FindStringSubmatch(m)
			if len(inner) < 3 {
				return m
			}
			code := reTag.ReplaceAllString(inner[2], "")
			return "\n```\n" + html.UnescapeString(code) + "\n```\n"
		})

		// Remove noisy structural sections.
		for _, re := range reNoisySections {
			htmlStr = re.ReplaceAllString(htmlStr, " ")
		}

		// Strip remaining HTML tags.
		htmlStr = reTag.ReplaceAllString(htmlStr, " ")

		// Decode entities and collapse whitespace.
		text := html.UnescapeString(htmlStr)
		text = reBlankLine.ReplaceAllString(text, "")
		text = reManyNewlines.ReplaceAllString(text, "\n\n")
		text = reManySpaces.ReplaceAllString(text, " ")

		const maxBytes = 30 * 1024 // 30 KB
		if len(text) > maxBytes {
			text = TruncateUTF8(text, maxBytes) + "\n... (truncated at 30 KB)"
		}
		return strings.TrimSpace(text), nil, nil

	default:
		return "", nil, fmt.Errorf("unknown tool: %s", call.Name)
	}
}

// RunCommand executes a run_command call directly via the sandbox.
// This is called by the TUI after user approval (or auto-approve).
// stdin may be nil (falls back to the default sudo-seed buffer via sandbox.Execute),
// or an io.Reader (e.g. an io.Pipe read-end for interactive mid-run input injection).
func RunCommand(ctx context.Context, call llm.ToolCall, sudoPassword string, stdin io.Reader) (string, error) {
	command, _ := call.Args["command"].(string)
	workingDir, _ := call.Args["working_dir"].(string)

	var timeoutDur time.Duration
	if v, ok := call.Args["timeout_seconds"].(float64); ok && v > 0 {
		timeoutDur = time.Duration(v) * time.Second
	}

	opts := sandbox.Options{
		WorkingDir:   workingDir,
		Timeout:      timeoutDur,
		SudoPassword: sudoPassword,
	}

	var result *sandbox.Result
	var err error
	if stdin != nil {
		result, err = sandbox.ExecuteWithStdin(ctx, command, stdin, opts)
	} else {
		result, err = sandbox.Execute(ctx, command, opts)
	}
	if err != nil {
		return "", err
	}
	return result.Combined(), nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

const (
	// maxHTTPBodyBytes caps how much of a web response is read into memory.
	maxHTTPBodyBytes = 5 << 20 // 5 MiB
	// maxSearchFileBytes: search_regex skips files larger than this.
	maxSearchFileBytes = 2 << 20 // 2 MiB
)

var (
	reTag          = regexp.MustCompile(`(?is)<[^>]*>`)
	reCodeBlock    = regexp.MustCompile(`(?is)<(pre|code)[^>]*>(.*?)</(pre|code)>`)
	reBlankLine    = regexp.MustCompile(`(?m)^\s+$`)
	reManyNewlines = regexp.MustCompile(`\n{3,}`)
	reManySpaces   = regexp.MustCompile(`[ \t]{2,}`)
	reDDGTitle     = regexp.MustCompile(`(?i)class=["']result-link["'][^>]*>(.*?)</a>`)
	reDDGURL       = regexp.MustCompile(`(?i)<a[^>]+class=["']result-link["'][^>]*href=["']([^"']+)["']`)
	reDDGSnippet   = regexp.MustCompile(`(?i)class=["']result-snippet["'][^>]*>(.*?)</td>`)

	reNoisySections = func() []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, tag := range []string{"script", "style", "nav", "header", "footer", "aside", "noscript"} {
			out = append(out, regexp.MustCompile(`(?is)<`+tag+`[^>]*>.*?</`+tag+`>`))
		}
		return out
	}()
)

// maxReadBytes caps how much of a file read_file returns at once.
const maxReadBytes = 50 * 1024

// numberedLines renders lines [start, end] (1-indexed, inclusive; 0 = open
// ended) of content with "%6d\t" line-number prefixes, stopping at
// maxReadBytes on a line boundary.
func numberedLines(path, content string, start, end int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // a trailing newline is not a line
	}
	total := len(lines)
	if total == 0 || (total == 1 && lines[0] == "") {
		return fmt.Sprintf("(%s is empty)", path)
	}
	if start < 1 {
		start = 1
	}
	if end <= 0 || end > total {
		end = total
	}
	if start > total {
		return fmt.Sprintf("(%s has only %d lines)", path, total)
	}
	if end < start {
		end = start
	}

	var b strings.Builder
	if start > 1 || end < total {
		fmt.Fprintf(&b, "(%s lines %d-%d of %d)\n", path, start, end, total)
	}
	last := start - 1
	for i := start; i <= end; i++ {
		line := lines[i-1]
		if len(line) > 2000 {
			line = TruncateUTF8(line, 2000) + "…(line truncated)"
		}
		entry := fmt.Sprintf("%6d\t%s\n", i, line)
		if b.Len()+len(entry) > maxReadBytes {
			break
		}
		b.WriteString(entry)
		last = i
	}
	out := strings.TrimRight(b.String(), "\n")
	if last < end {
		out += fmt.Sprintf("\n… (output truncated at line %d of %d — continue with start_line=%d)", last, total, last+1)
	}
	return out
}

// TruncateUTF8 cuts s to at most max bytes without splitting a multi-byte rune.
func TruncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// looksBinary reports whether data appears to be a binary file (NUL byte in
// the first 8 KB), which search_regex skips.
func looksBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

// toolPathArgs lists, per read-only filesystem tool, which argument holds the
// path and its default.
var toolPathArgs = map[string][2]string{
	"read_file":      {"path", ""},
	"list_directory": {"path", "."},
	"search_regex":   {"dir", "."},
	"find_files":     {"dir", "."},
	"map_workspace":  {"dir", "."},
	"repo_map":       {"dir", "."},
}

// NeedsApproval decides whether a native (non run_command,
// non-write) tool call must be confirmed by the user before it runs.
//
// Reading inside the workspace root runs freely. Reading outside it, and
// fetching arbitrary URLs, need approval: together, unrestricted reads and
// unrestricted outbound requests let injected instructions (from a web page or
// a file) exfiltrate secrets such as ~/.ssh keys.
func NeedsApproval(call llm.ToolCall, workspaceRoot string) (bool, string) {
	if call.Name == "web_fetch" || call.Name == "web_scrape" {
		u, _ := call.Args["url"].(string)
		return true, "fetch URL: " + u
	}
	if spec, ok := toolPathArgs[call.Name]; ok {
		p, _ := call.Args[spec[0]].(string)
		if p == "" {
			p = spec[1]
		}
		if p == "" || workspaceRoot == "" {
			return false, ""
		}
		if !workspace.IsInside(workspaceRoot, p) {
			return true, "access outside the workspace: " + p
		}
	}
	return false, ""
}

// intArg extracts an integer from a tool args map, handling both int and float64
// (JSON numbers are always float64 after unmarshalling).
func intArg(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

// simpleDiff produces a minimal line-level diff string between old and new content.
// It uses a simple approach: mark removed lines with "- " and added lines with "+ ".
func simpleDiff(oldContent, newContent string) string {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	// Find first differing line.
	firstDiff := 0
	for firstDiff < len(oldLines) && firstDiff < len(newLines) {
		if oldLines[firstDiff] != newLines[firstDiff] {
			break
		}
		firstDiff++
	}

	// Find last differing line (from the end).
	lastOld := len(oldLines) - 1
	lastNew := len(newLines) - 1
	for lastOld > firstDiff && lastNew > firstDiff {
		if oldLines[lastOld] != newLines[lastNew] {
			break
		}
		lastOld--
		lastNew--
	}

	if firstDiff > lastOld && firstDiff > lastNew {
		return "(no changes)"
	}

	const contextLines = 2
	var sb strings.Builder

	// Context before.
	ctxStart := firstDiff - contextLines
	if ctxStart < 0 {
		ctxStart = 0
	}
	for i := ctxStart; i < firstDiff; i++ {
		sb.WriteString("  " + oldLines[i] + "\n")
	}

	// Removed lines.
	for i := firstDiff; i <= lastOld && i < len(oldLines); i++ {
		sb.WriteString("- " + oldLines[i] + "\n")
	}

	// Added lines.
	for i := firstDiff; i <= lastNew && i < len(newLines); i++ {
		sb.WriteString("+ " + newLines[i] + "\n")
	}

	// Context after.
	ctxEnd := lastNew + contextLines + 1
	if ctxEnd > len(newLines) {
		ctxEnd = len(newLines)
	}
	for i := lastNew + 1; i < ctxEnd; i++ {
		sb.WriteString("  " + newLines[i] + "\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}
