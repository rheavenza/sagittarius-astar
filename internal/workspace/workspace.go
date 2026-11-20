// Package workspace provides helpers for reasoning about the user's working
// directory: tree mapping, ignored directories, and path containment checks.
package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Limits for MapDirectory so mapping a huge tree (e.g. ~) can't flood the
// LLM context.
const (
	MaxMapEntries = 2000
	MaxMapDepth   = 8
)

// ignoredDirs are skipped by tree mapping and recursive search.
var ignoredDirs = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"bin":          true,
	".gocache":     true,
	"dist":         true,
	"build":        true,
	"target":       true,
	"__pycache__":  true,
	".venv":        true,
	".direnv":      true,
}

// IsIgnoredDir reports whether a directory with this base name should be
// skipped during recursive traversal (VCS metadata, dependency and build dirs).
func IsIgnoredDir(name string) bool {
	return ignoredDirs[name]
}

// MapDirectory returns a tree-like string representation of the given directory.
// It skips common ignored directories, unreadable directories, and stops at
// MaxMapDepth / MaxMapEntries.
func MapDirectory(root string) (string, error) {
	if info, err := os.Stat(root); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", fmt.Errorf("map_workspace: %s is not a directory", root)
	}

	var builder strings.Builder
	entries := 0
	truncated := false

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entry: note it and keep going instead of aborting.
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			if path == root {
				return err
			}
			return nil
		}

		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if relPath == "." {
			builder.WriteString(fmt.Sprintf("%s/\n", filepath.Base(root)))
			return nil
		}

		if d.IsDir() && IsIgnoredDir(d.Name()) {
			return filepath.SkipDir
		}

		depth := strings.Count(relPath, string(os.PathSeparator))
		if depth >= MaxMapDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if entries >= MaxMapEntries {
			truncated = true
			return filepath.SkipAll
		}
		entries++

		name := d.Name()
		if d.IsDir() {
			name += "/"
		}
		builder.WriteString(strings.Repeat("  ", depth) + name + "\n")
		return nil
	})
	if err != nil {
		return "", err
	}

	if truncated {
		builder.WriteString(fmt.Sprintf("... (truncated at %d entries — map a subdirectory for more detail)\n", MaxMapEntries))
	}
	return builder.String(), nil
}

// IsInside reports whether path (after resolving symlinks where possible)
// lies within root. Both are made absolute first.
func IsInside(root, path string) bool {
	absRoot, err := resolve(root)
	if err != nil {
		return false
	}
	absPath, err := resolve(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// resolve makes p absolute and resolves symlinks for the longest existing
// prefix, so a not-yet-existing file under a symlinked dir still resolves.
func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	dir, base := filepath.Split(abs)
	dir = filepath.Clean(dir)
	if dir == abs {
		return abs, nil
	}
	parent, err := resolve(dir)
	if err != nil {
		return abs, nil
	}
	return filepath.Join(parent, base), nil
}
