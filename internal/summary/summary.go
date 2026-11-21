// Package summary builds a cached, hierarchical summary of a repository:
// one short summary per source file, then per directory, then a project
// overview (map-reduce on an LLM, preferably a cheap one).
//
// Summaries are cached by content hash in <user cache>/aig/index/<repo>/,
// so a re-run only pays for files that changed (and the directories above
// them). The index is read by the project_overview tool and the system
// prompt.
package summary

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

// promptVersion is part of every cache key: bump it when the prompts change
// so old summaries are regenerated.
const promptVersion = "1"

const (
	// maxSourceBytes: larger files are skipped (generated code, data).
	maxSourceBytes = 256 * 1024
	// maxSentBytes of a file are sent to the model.
	maxSentBytes = 24 * 1024
	// maxFiles bounds one index.
	maxFiles = 5000
)

// FileEntry is a summarized file.
type FileEntry struct {
	Hash    string `json:"hash"`
	Summary string `json:"summary"`
}

// Index is the stored summary of a repository.
type Index struct {
	Version string               `json:"version"`
	Root    string               `json:"root"`
	Model   string               `json:"model,omitempty"`
	Updated time.Time            `json:"updated"`
	Project FileEntry            `json:"project"`
	Dirs    map[string]FileEntry `json:"dirs"`  // "internal/agent" → summary; Hash covers its children
	Files   map[string]FileEntry `json:"files"` // "internal/agent/agent.go"
}

// RootFor returns the repository root for dir: the git top level, or dir
// itself (absolute) outside git.
func RootFor(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	out, err := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return top
		}
	}
	return abs
}

// Dir is where the index for root is stored.
func Dir(root string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "aig-cache")
	}
	sum := sha256.Sum256([]byte(root))
	name := filepath.Base(root) + "-" + hex.EncodeToString(sum[:])[:12]
	return filepath.Join(base, "aig", "index", name)
}

func indexPath(root string) string { return filepath.Join(Dir(root), "index.json") }

// MarkdownPath is the rendered PROJECT_SUMMARY.md for root.
func MarkdownPath(root string) string { return filepath.Join(Dir(root), "PROJECT_SUMMARY.md") }

// Exists reports whether root has a completed index.
func Exists(root string) bool {
	_, err := os.Stat(MarkdownPath(root))
	return err == nil
}

// Load reads the index for root. A missing index returns (nil, nil).
func Load(root string) (*Index, error) {
	data, err := os.ReadFile(indexPath(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, fmt.Errorf("summary index %s: %w", indexPath(root), err)
	}
	if ix.Version != promptVersion {
		return nil, nil // stale format: start over
	}
	if ix.Dirs == nil {
		ix.Dirs = map[string]FileEntry{}
	}
	if ix.Files == nil {
		ix.Files = map[string]FileEntry{}
	}
	return &ix, nil
}

// save writes the index atomically.
func (ix *Index) save() error {
	if err := os.MkdirAll(Dir(ix.Root), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ix, "", " ")
	if err != nil {
		return err
	}
	tmp := indexPath(ix.Root) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, indexPath(ix.Root))
}

// ── which files ──────────────────────────────────────────────────────────────

var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".ts": true, ".tsx": true, ".mts": true, ".rs": true, ".sh": true, ".bash": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".java": true,
	".kt": true, ".swift": true, ".rb": true, ".php": true, ".lua": true, ".zig": true,
	".cs": true, ".scala": true, ".ex": true, ".exs": true, ".hs": true, ".ml": true,
	".sql": true, ".nix": true, ".proto": true, ".vue": true, ".svelte": true,
}

var reGenerated = regexp.MustCompile(`(?m)^(//|#|--) Code generated .* DO NOT EDIT\.?$`)

// isTestFile: tests are skipped to save cost; the code they test is what
// the summary is for.
func isTestFile(p string) bool {
	base := path.Base(p)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.Contains("/"+p, "/tests/") ||
		strings.Contains("/"+p, "/__tests__/") || strings.Contains("/"+p, "/testdata/")
}

// candidate is a file to summarize with its current content hash.
type candidate struct {
	rel  string
	hash string
	size int
}

// sourceFiles lists the summarizable files under root.
func sourceFiles(root string) ([]candidate, bool, error) {
	all, _, err := workspace.ListFiles(root, 200_000)
	if err != nil {
		return nil, false, err
	}
	var out []candidate
	truncated := false
	for _, rel := range all {
		if !sourceExts[strings.ToLower(path.Ext(rel))] || isTestFile(rel) || strings.HasSuffix(rel, ".min.js") {
			continue
		}
		skip := false
		for _, part := range strings.Split(rel, "/") {
			if workspace.IsIgnoredDir(part) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || len(data) == 0 || len(data) > maxSourceBytes || looksBinary(data) {
			continue
		}
		head := data
		if len(head) > 4096 {
			head = head[:4096]
		}
		if reGenerated.Match(head) {
			continue
		}
		if len(out) >= maxFiles {
			truncated = true
			break
		}
		out = append(out, candidate{rel: rel, hash: hashOf("file", rel, string(data)), size: len(data)})
	}
	return out, truncated, nil
}

func looksBinary(data []byte) bool {
	n := min(len(data), 8000)
	for _, b := range data[:n] {
		if b == 0 {
			return true
		}
	}
	return false
}

func hashOf(parts ...string) string {
	h := sha256.New()
	h.Write([]byte(promptVersion))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ── plan ─────────────────────────────────────────────────────────────────────

// Plan says what a run would do.
type Plan struct {
	Root      string
	Files     int   // summarizable files
	Cached    int   // of which already summarized with this content
	Todo      int   // files to (re)summarize
	TodoBytes int64 // bytes of source those calls send (capped per file)
	Removed   int   // indexed files that no longer exist
	UpToDate  bool  // nothing to do, project summary present
	Truncated bool  // more than maxFiles source files; the rest are ignored

	files []candidate
	index *Index
}

// Scan compares the source files with the stored index.
func Scan(root string) (*Plan, error) {
	files, truncated, err := sourceFiles(root)
	if err != nil {
		return nil, err
	}
	ix, err := Load(root)
	if err != nil {
		return nil, err
	}
	if ix == nil {
		ix = &Index{Version: promptVersion, Root: root, Dirs: map[string]FileEntry{}, Files: map[string]FileEntry{}}
	}
	p := &Plan{Root: root, Files: len(files), Truncated: truncated, files: files, index: ix}
	live := map[string]bool{}
	for _, f := range files {
		live[f.rel] = true
		if e, ok := ix.Files[f.rel]; ok && e.Hash == f.hash {
			p.Cached++
		} else {
			p.Todo++
			p.TodoBytes += int64(min(f.size, maxSentBytes))
		}
	}
	for rel := range ix.Files {
		if !live[rel] {
			p.Removed++
		}
	}
	p.UpToDate = p.Todo == 0 && p.Removed == 0 && ix.Project.Summary != "" && Exists(root)
	return p, nil
}

// ── reading the index ────────────────────────────────────────────────────────

// Overview returns the project summary ("" if none).
func (ix *Index) Overview() string { return ix.Project.Summary }

// sortedKeys returns m's keys sorted.
func sortedKeys(m map[string]FileEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Markdown renders PROJECT_SUMMARY.md.
func (ix *Index) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Project summary: %s\n\n", filepath.Base(ix.Root))
	fmt.Fprintf(&b, "_Generated by aig /summarize on %s", ix.Updated.Format("2006-01-02 15:04"))
	if ix.Model != "" {
		fmt.Fprintf(&b, " with %s", ix.Model)
	}
	b.WriteString(". Summaries are model-written; verify details in the code._\n\n")
	b.WriteString(strings.TrimSpace(ix.Project.Summary) + "\n")
	for _, dir := range sortedKeys(ix.Dirs) {
		if dir == "." {
			continue
		}
		fmt.Fprintf(&b, "\n## %s/\n\n%s\n", dir, strings.TrimSpace(ix.Dirs[dir].Summary))
		for _, f := range ix.filesIn(dir) {
			fmt.Fprintf(&b, "- `%s`: %s\n", path.Base(f), oneLine(ix.Files[f].Summary))
		}
	}
	if root := ix.filesIn("."); len(root) > 0 {
		b.WriteString("\n## Top-level files\n\n")
		for _, f := range root {
			fmt.Fprintf(&b, "- `%s`: %s\n", f, oneLine(ix.Files[f].Summary))
		}
	}
	return b.String()
}

// filesIn lists indexed files directly inside dir, sorted.
func (ix *Index) filesIn(dir string) []string {
	var out []string
	for f := range ix.Files {
		if path.Dir(f) == dir {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// subdirsOf lists indexed directories directly inside dir, sorted.
func (ix *Index) subdirsOf(dir string) []string {
	var out []string
	for d := range ix.Dirs {
		if d != "." && path.Dir(d) == dir {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
