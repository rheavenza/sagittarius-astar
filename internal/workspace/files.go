package workspace

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ListFiles returns the files under root as slash-separated paths
// relative to root. Inside a git work tree it uses `git ls-files`, which
// respects .gitignore and includes untracked-but-not-ignored files; otherwise
// it walks the tree, skipping VCS/dependency/build and hidden directories.
// At most limit files are returned (truncated reports whether more exist).
func ListFiles(root string, limit int) ([]string, bool, error) {
	if files, ok := gitListFiles(root); ok {
		truncated := false
		if len(files) > limit {
			files, truncated = files[:limit], true
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
			if path != root && (IsIgnoredDir(d.Name()) || strings.HasPrefix(d.Name(), ".")) {
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
		if len(files) >= limit {
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
