package tui

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

const (
	maxFindResults   = 200
	maxSearchMatches = 50
	maxListedFiles   = 100_000
	searchTimeout    = 20 * time.Second
)

// listProjectFiles returns the files under root as slash-separated paths
// relative to root. Inside a git work tree it uses `git ls-files`, which
// respects .gitignore and includes untracked-but-not-ignored files; otherwise
// it walks the tree, skipping VCS/dependency/build and hidden directories.
func listProjectFiles(root string) ([]string, bool, error) {
	if files, ok := gitListFiles(root); ok {
		truncated := false
		if len(files) > maxListedFiles {
			files, truncated = files[:maxListedFiles], true
		}
		return files, truncated, nil
	}

	var files []string
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			if path == root {
				return err
			}
			return nil
		}
		if d.IsDir() {
			if path != root && (workspace.IsIgnoredDir(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= maxListedFiles {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})
	return files, truncated, err
}

// gitListFiles lists files via git, or reports ok=false if root isn't in a
// git work tree (or git is unavailable).
func gitListFiles(root string) ([]string, bool) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	out, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) == 0 {
			continue
		}
		rel := string(p)
		// --cached includes tracked files deleted from the work tree.
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			continue
		}
		files = append(files, rel)
	}
	sort.Strings(files)
	return files, true
}

// globToRegexp converts a glob with `**`, `*`, `?`, `[...]` and `{a,b}`
// support into an anchored regexp over slash-separated paths.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	braces := 0
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?") // "**/" = zero or more directories
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '{':
			braces++
			b.WriteString("(?:")
		case '}':
			if braces > 0 {
				braces--
				b.WriteString(")")
			} else {
				b.WriteString(`\}`)
			}
		case ',':
			if braces > 0 {
				b.WriteString("|")
			} else {
				b.WriteString(",")
			}
		case '[':
			if j := strings.IndexByte(pattern[i:], ']'); j > 1 {
				class := pattern[i+1 : i+j]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("[" + class + "]")
				i += j
			} else {
				b.WriteString(`\[`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	if braces > 0 {
		return nil, fmt.Errorf("unbalanced '{' in glob %q", pattern)
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// globMatcher returns a predicate over slash-separated relative paths. A
// pattern without "/" matches the file's base name anywhere in the tree
// (so "*.go" finds every Go file), like .gitignore semantics.
func globMatcher(pattern string) (func(string) bool, error) {
	re, err := globToRegexp(pattern)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(pattern, "/") {
		return func(p string) bool { return re.MatchString(pathBase(p)) }, nil
	}
	return re.MatchString, nil
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// findFiles implements the find_files tool.
func findFiles(pattern, dir string) (string, error) {
	match, err := globMatcher(pattern)
	if err != nil {
		return "", fmt.Errorf("find_files: %w", err)
	}
	files, truncatedList, err := listProjectFiles(dir)
	if err != nil {
		return "", fmt.Errorf("find_files: %w", err)
	}
	var hits []string
	for _, f := range files {
		if match(f) {
			hits = append(hits, f)
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No files matching %q under %s", pattern, dir), nil
	}
	total := len(hits)
	if total > maxFindResults {
		hits = hits[:maxFindResults]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d file(s) matching %q under %s", total, pattern, dir)
	if total > maxFindResults {
		fmt.Fprintf(&b, " (showing first %d — narrow the pattern)", maxFindResults)
	}
	if truncatedList {
		b.WriteString(" (file listing truncated)")
	}
	b.WriteString(":\n")
	for _, h := range hits {
		b.WriteString(joinDisplay(dir, h) + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// joinDisplay renders a listed path relative to the cwd when dir is ".".
func joinDisplay(dir, rel string) string {
	if dir == "." || dir == "" {
		return rel
	}
	return filepath.ToSlash(filepath.Join(dir, rel))
}

// searchFiles implements search_regex: ripgrep when available (fast,
// .gitignore-aware, skips binaries), otherwise a Go fallback over
// listProjectFiles.
func searchFiles(pattern, dir, fileGlob string, caseInsensitive bool) (string, error) {
	if caseInsensitive && !strings.HasPrefix(pattern, "(?i)") {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("search_regex: invalid pattern %q: %w", pattern, err)
	}

	var matches []string
	truncated := false
	if rg, err := exec.LookPath("rg"); err == nil {
		matches, truncated, err = ripgrep(rg, pattern, dir, fileGlob)
		if err != nil {
			return "", err
		}
	} else {
		matches, truncated, err = goSearch(re, dir, fileGlob)
		if err != nil {
			return "", err
		}
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No matches found for pattern %q in %s", pattern, dir), nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n... (truncated at %d matches — narrow the pattern or use file_glob)", maxSearchMatches)
	}
	return out, nil
}

func ripgrep(rg, pattern, dir, fileGlob string) ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	defer cancel()
	args := []string{"--line-number", "--no-heading", "--color=never", "--max-columns=300", "--max-columns-preview"}
	if fileGlob != "" {
		args = append(args, "--glob", fileGlob)
	}
	args = append(args, "--regexp", pattern, "--", dir)
	cmd := exec.CommandContext(ctx, rg, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}

	var matches []string
	truncated := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if len(matches) >= maxSearchMatches {
			truncated = true
			cancel() // stop rg early
			break
		}
		matches = append(matches, formatMatchLine(sc.Text()))
	}
	_ = cmd.Wait()
	// rg exits 1 for "no matches" and 2 for errors.
	if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 2 && len(matches) == 0 {
		return nil, false, fmt.Errorf("search_regex: %s", strings.TrimSpace(stderr.String()))
	}
	return matches, truncated, nil
}

// formatMatchLine turns rg's "path:line:text" into "path:line: text".
func formatMatchLine(s string) string {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return s
	}
	return parts[0] + ":" + parts[1] + ": " + strings.TrimSpace(parts[2])
}

func goSearch(re *regexp.Regexp, dir, fileGlob string) ([]string, bool, error) {
	var match func(string) bool
	if fileGlob != "" {
		m, err := globMatcher(fileGlob)
		if err != nil {
			return nil, false, fmt.Errorf("search_regex: %w", err)
		}
		match = m
	}
	files, _, err := listProjectFiles(dir)
	if err != nil {
		return nil, false, fmt.Errorf("search_regex: %w", err)
	}

	var matches []string
	for _, rel := range files {
		if match != nil && !match(rel) {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil || info.Size() > maxSearchFileBytes {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil || looksBinary(data) {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			line = strings.TrimSpace(line)
			if len(line) > 300 {
				line = truncateUTF8(line, 300) + "…"
			}
			matches = append(matches, fmt.Sprintf("%s:%d: %s", joinDisplay(dir, rel), i+1, line))
			if len(matches) >= maxSearchMatches {
				return matches, true, nil
			}
		}
	}
	return matches, false, nil
}
