package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const gitTimeout = 60 * time.Second

// gitRepo runs git plumbing against the repository containing a workspace.
type gitRepo struct {
	top    string // work tree root
	prefix string // workspace root relative to top ("" if equal)
}

// openGit returns a gitRepo if root is inside a git work tree.
func openGit(root string) *gitRepo {
	if _, err := exec.LookPath("git"); err != nil {
		return nil
	}
	g := &gitRepo{}
	top, err := g.run(root, nil, nil, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return nil
	}
	g.top = filepath.Clean(top)
	if rel, err := filepath.Rel(g.top, root); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		g.prefix = filepath.ToSlash(rel)
	}
	return g
}

// identityEnv lets commit-tree work without a configured user and keeps
// checkpoint commits recognizable.
var identityEnv = []string{
	"GIT_AUTHOR_NAME=aig", "GIT_AUTHOR_EMAIL=aig@localhost",
	"GIT_COMMITTER_NAME=aig", "GIT_COMMITTER_EMAIL=aig@localhost",
}

func (g *gitRepo) run(dir string, env []string, stdin io.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), identityEnv...), env...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// pathspec limits commands to the workspace when it is a subdirectory.
func (g *gitRepo) pathspec() []string {
	if g.prefix == "" {
		return []string{"."}
	}
	return []string{g.prefix}
}

// tempIndex returns GIT_INDEX_FILE env for a private copy of the real index
// (copying makes `add -A` fast: unchanged files keep their cached stat data).
func (g *gitRepo) tempIndex() ([]string, func(), error) {
	f, err := os.CreateTemp("", "aig-index-*")
	if err != nil {
		return nil, nil, err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if idx, err := g.run(g.top, nil, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"); err == nil {
		if src, err := os.Open(idx); err == nil {
			_, _ = io.Copy(f, src)
			src.Close()
		}
	}
	f.Close()
	return []string{"GIT_INDEX_FILE=" + tmp}, cleanup, nil
}

// snapshot commits the current working tree (tracked + untracked, not
// ignored) without touching the user's index, HEAD or branches.
func (g *gitRepo) snapshot(message string) (string, error) {
	env, cleanup, err := g.tempIndex()
	if err != nil {
		return "", err
	}
	defer cleanup()

	if _, err := g.run(g.top, env, nil, append([]string{"add", "-A", "--"}, g.pathspec()...)...); err != nil {
		return "", err
	}
	tree, err := g.run(g.top, env, nil, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree, "-m", message}
	if head, err := g.run(g.top, nil, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil && head != "" {
		args = append(args, "-p", head)
	}
	return g.run(g.top, nil, nil, args...)
}

func (g *gitRepo) updateRef(ref, commit string) error {
	_, err := g.run(g.top, nil, nil, "update-ref", ref, commit)
	return err
}

func (g *gitRepo) deleteRef(ref string) error {
	_, err := g.run(g.top, nil, nil, "update-ref", "-d", ref)
	return err
}

// listRefs returns ref-name-suffix → commit for refs under prefix.
func (g *gitRepo) listRefs(prefix string) (map[string]string, error) {
	out, err := g.run(g.top, nil, nil, "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		name, commit, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		refs[strings.TrimPrefix(name, prefix)] = commit
	}
	return refs, nil
}

// commitInfo returns the label (from the message) and time of a checkpoint.
func (g *gitRepo) commitInfo(commit string) (string, time.Time) {
	out, err := g.run(g.top, nil, nil, "log", "-1", "--format=%ct%x00%s", commit)
	if err != nil {
		return "", time.Time{}
	}
	ts, subject, _ := strings.Cut(out, "\x00")
	var when time.Time
	if sec, err := parseInt(ts); err == nil {
		when = time.Unix(sec, 0)
	}
	if _, label, ok := strings.Cut(subject, ": "); ok {
		subject = label
	}
	return subject, when
}

func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscan(s, &n)
	return n, err
}

// restoreTree makes the workspace files match target, given a snapshot of
// the current state. It returns paths (relative to the workspace root) that
// were restored or deleted.
func (g *gitRepo) restoreTree(target, current, root string) ([]string, []string, error) {
	out, err := g.run(g.top, nil, nil, append([]string{"diff", "--name-status", "--no-renames", "-z", target, current, "--"}, g.pathspec()...)...)
	if err != nil {
		return nil, nil, err
	}
	fields := strings.Split(out, "\x00")
	var restore, remove []string
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		if status == "" || path == "" {
			continue
		}
		if status == "A" { // added after the checkpoint
			remove = append(remove, path)
		} else {
			restore = append(restore, path)
		}
	}

	var restored, deleted []string
	for _, p := range remove {
		full := filepath.Join(g.top, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return restored, deleted, err
		}
		removeEmptyParents(filepath.Dir(full), g.top)
		deleted = append(deleted, g.relToRoot(p, root))
	}

	if len(restore) > 0 {
		env, cleanup, err := g.tempIndex()
		if err != nil {
			return restored, deleted, err
		}
		defer cleanup()
		if _, err := g.run(g.top, env, nil, "read-tree", target); err != nil {
			return restored, deleted, err
		}
		stdin := strings.NewReader(strings.Join(restore, "\x00") + "\x00")
		if _, err := g.run(g.top, env, stdin, "checkout-index", "-f", "-z", "--stdin"); err != nil {
			return restored, deleted, err
		}
		for _, p := range restore {
			restored = append(restored, g.relToRoot(p, root))
		}
	}
	return restored, deleted, nil
}

// diff renders the changes between two snapshots (stat + patch).
func (g *gitRepo) diff(base, current, root string) (string, error) {
	args := append([]string{"diff", "--no-color", "--stat", "--patch", "--no-ext-diff", base, current, "--"}, g.pathspec()...)
	out, err := g.run(g.top, nil, nil, args...)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "(no changes since the session's first checkpoint)", nil
	}
	return out, nil
}

func (g *gitRepo) relToRoot(repoPath, root string) string {
	full := filepath.Join(g.top, filepath.FromSlash(repoPath))
	if r, err := filepath.Rel(root, full); err == nil {
		return filepath.ToSlash(r)
	}
	return repoPath
}

// removeEmptyParents deletes now-empty directories up to (not including) stop.
func removeEmptyParents(dir, stop string) {
	for dir != stop && strings.HasPrefix(dir, stop) {
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
