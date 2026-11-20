// Package checkpoint snapshots the workspace before the agent changes it, so
// a turn's changes can be undone and redone.
//
// Git mode (workspace inside a git work tree): each checkpoint is a commit
// of the whole working tree — tracked files plus untracked files that are not
// ignored — built with a temporary index and stored under the hidden ref
// refs/aig/checkpoints/<session>/<seq>. The user's index, HEAD and branches
// are never touched, and checkpoints survive restarts.
//
// Journal mode (no git): the original content of every file written through
// aig's write tools is kept in memory. Changes made by shell commands can't
// be undone in this mode.
package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxCheckpoints is how many checkpoint refs are kept per session.
const maxCheckpoints = 50

// Info describes a checkpoint for display.
type Info struct {
	Seq   int
	Label string
	Time  time.Time
}

// Result reports what an undo/redo changed.
type Result struct {
	Checkpoint Info
	Restored   []string // files rewritten to their checkpoint content
	Deleted    []string // files that did not exist at the checkpoint
}

// Changed reports whether anything was modified.
func (r *Result) Changed() bool { return len(r.Restored)+len(r.Deleted) > 0 }

// fileState is a journal-mode backup of one file.
type fileState struct {
	content []byte
	existed bool
	mode    os.FileMode
}

type checkpoint struct {
	Info
	commit string                // git mode
	files  map[string]*fileState // journal mode: abs path → state at checkpoint
}

// Manager owns the checkpoints of one session. It is safe for concurrent use.
type Manager struct {
	mu      sync.Mutex
	root    string // workspace root (absolute)
	git     *gitRepo
	session string
	stack   []*checkpoint // undo stack (oldest first)
	redo    []*checkpoint
	base    *checkpoint // first checkpoint of the session (for /diff)
	seq     int
}

// New creates a Manager for root. It uses git mode when root is inside a git
// work tree, and loads any existing checkpoints of session.
func New(root, session string) *Manager {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	// Resolve symlinks so paths compare equal to git's (e.g. /home is a
	// symlink to /var/home on Fedora Atomic / Bazzite).
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	m := &Manager{root: abs, git: openGit(abs)}
	m.SetSession(session)
	return m
}

// Mode returns "git" or "journal".
func (m *Manager) Mode() string {
	if m.git != nil {
		return "git"
	}
	return "journal"
}

// SetSession switches to another session's checkpoints (e.g. /new, /load).
func (m *Manager) SetSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.session = sanitizeRef(id)
	m.stack, m.redo, m.base, m.seq = nil, nil, nil, 0
	if m.git == nil {
		return
	}
	refs, err := m.git.listRefs(m.refPrefix())
	if err != nil {
		return
	}
	for name, commit := range refs {
		if name == "base" {
			m.base = &checkpoint{Info: Info{Label: "session start"}, commit: commit}
			continue
		}
		seq, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		label, when := m.git.commitInfo(commit)
		m.stack = append(m.stack, &checkpoint{Info: Info{Seq: seq, Label: label, Time: when}, commit: commit})
		if seq > m.seq {
			m.seq = seq
		}
	}
	sort.Slice(m.stack, func(i, j int) bool { return m.stack[i].Seq < m.stack[j].Seq })
}

func (m *Manager) refPrefix() string { return "refs/aig/checkpoints/" + m.session + "/" }

// Checkpoint records the current state of the workspace. label describes
// the turn about to change it.
func (m *Manager) Checkpoint(label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	cp := &checkpoint{Info: Info{Seq: m.seq, Label: oneLine(label), Time: time.Now()}}
	if m.git != nil {
		commit, err := m.git.snapshot(fmt.Sprintf("aig checkpoint #%d: %s", m.seq, cp.Label))
		if err != nil {
			m.seq--
			return err
		}
		cp.commit = commit
		if err := m.git.updateRef(m.refPrefix()+strconv.Itoa(m.seq), commit); err != nil {
			m.seq--
			return err
		}
		if m.base == nil {
			m.base = &checkpoint{Info: cp.Info, commit: commit}
			_ = m.git.updateRef(m.refPrefix()+"base", commit)
		}
	} else {
		cp.files = map[string]*fileState{}
		if m.base == nil {
			m.base = &checkpoint{Info: cp.Info, files: map[string]*fileState{}}
		}
	}

	m.stack = append(m.stack, cp)
	m.redo = nil // a new change invalidates redo
	m.prune()
	return nil
}

// prune keeps at most maxCheckpoints checkpoints.
func (m *Manager) prune() {
	for len(m.stack) > maxCheckpoints {
		old := m.stack[0]
		m.stack = m.stack[1:]
		if m.git != nil {
			_ = m.git.deleteRef(m.refPrefix() + strconv.Itoa(old.Seq))
		}
	}
}

// RecordWrite remembers a file's content before an agent write. Only used in
// journal mode (git snapshots already cover it). The first record per file
// per checkpoint wins.
func (m *Manager) RecordWrite(path, original string, existed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.git != nil || len(m.stack) == 0 {
		return
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		abs = filepath.Join(real, filepath.Base(abs))
	}
	st := &fileState{content: []byte(original), existed: existed, mode: 0o644}
	if info, err := os.Stat(abs); err == nil {
		st.mode = info.Mode().Perm()
	}
	if cur := m.stack[len(m.stack)-1]; cur.files[abs] == nil {
		cur.files[abs] = st
	}
	if m.base != nil && m.base.files[abs] == nil {
		m.base.files[abs] = st
	}
}

// List returns the undo stack, newest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.stack))
	for i := len(m.stack) - 1; i >= 0; i-- {
		out = append(out, m.stack[i].Info)
	}
	return out
}

// CanRedo reports whether Redo has something to restore.
func (m *Manager) CanRedo() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.redo) > 0
}

// Undo restores the workspace to the most recent checkpoint (the state before
// the last agent turn that changed files). The current state becomes a redo
// point first, so nothing is lost.
func (m *Manager) Undo() (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.stack) == 0 {
		return nil, fmt.Errorf("nothing to undo: no checkpoints in this session")
	}
	target := m.stack[len(m.stack)-1]

	redoPoint, res, err := m.restore(target)
	if err != nil {
		return nil, err
	}
	m.stack = m.stack[:len(m.stack)-1]
	if m.git != nil {
		_ = m.git.deleteRef(m.refPrefix() + strconv.Itoa(target.Seq))
	}
	m.redo = append(m.redo, redoPoint)
	return res, nil
}

// Redo re-applies the changes most recently undone.
func (m *Manager) Redo() (*Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.redo) == 0 {
		return nil, fmt.Errorf("nothing to redo")
	}
	target := m.redo[len(m.redo)-1]

	undoPoint, res, err := m.restore(target)
	if err != nil {
		return nil, err
	}
	m.redo = m.redo[:len(m.redo)-1]
	// The pre-redo state becomes an undo checkpoint again.
	m.seq++
	undoPoint.Seq = m.seq
	if m.git != nil {
		_ = m.git.updateRef(m.refPrefix()+strconv.Itoa(m.seq), undoPoint.commit)
	}
	m.stack = append(m.stack, undoPoint)
	return res, nil
}

// restore makes the workspace match target and returns a checkpoint of the
// state it replaced.
func (m *Manager) restore(target *checkpoint) (*checkpoint, *Result, error) {
	res := &Result{Checkpoint: target.Info}
	saved := &checkpoint{Info: Info{Label: target.Label, Time: time.Now()}}

	if m.git != nil {
		current, err := m.git.snapshot("aig restore point: " + target.Label)
		if err != nil {
			return nil, nil, err
		}
		saved.commit = current
		restored, deleted, err := m.git.restoreTree(target.commit, current, m.root)
		if err != nil {
			return nil, nil, err
		}
		res.Restored, res.Deleted = restored, deleted
		return saved, res, nil
	}

	// Journal mode: capture current contents of the same files, then restore.
	saved.files = map[string]*fileState{}
	paths := make([]string, 0, len(target.files))
	for p := range target.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		cur := &fileState{mode: 0o644}
		if data, err := os.ReadFile(p); err == nil {
			cur.content, cur.existed = data, true
			if info, err := os.Stat(p); err == nil {
				cur.mode = info.Mode().Perm()
			}
		}
		saved.files[p] = cur

		want := target.files[p]
		rel := m.rel(p)
		if !want.existed {
			if cur.existed {
				if err := os.Remove(p); err != nil {
					return nil, nil, err
				}
				res.Deleted = append(res.Deleted, rel)
			}
			continue
		}
		if cur.existed && string(cur.content) == string(want.content) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(p, want.content, want.mode); err != nil {
			return nil, nil, err
		}
		res.Restored = append(res.Restored, rel)
	}
	return saved, res, nil
}

// Diff shows every change since the first checkpoint of the session.
func (m *Manager) Diff() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.base == nil {
		return "", fmt.Errorf("no checkpoints yet in this session — nothing has been changed by the agent")
	}
	if m.git != nil {
		current, err := m.git.snapshot("aig diff point")
		if err != nil {
			return "", err
		}
		return m.git.diff(m.base.commit, current, m.root)
	}

	var b strings.Builder
	paths := make([]string, 0, len(m.base.files))
	for p := range m.base.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		orig := m.base.files[p]
		cur, err := os.ReadFile(p)
		exists := err == nil
		if exists == orig.existed && string(cur) == string(orig.content) {
			continue
		}
		fmt.Fprintf(&b, "--- %s\n", m.rel(p))
		switch {
		case !orig.existed:
			b.WriteString("(new file)\n")
		case !exists:
			b.WriteString("(deleted)\n")
		default:
			b.WriteString(lineDiff(string(orig.content), string(cur)))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (m *Manager) rel(p string) string {
	if r, err := filepath.Rel(m.root, p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}

func oneLine(s string) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(s) > 72 {
		cut := 69
		for cut > 0 && (s[cut]&0xC0) == 0x80 {
			cut--
		}
		s = s[:cut] + "..."
	}
	if s == "" {
		s = "agent turn"
	}
	return s
}

// sanitizeRef makes s safe as a git ref path component.
func sanitizeRef(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// lineDiff is a compact changed-lines diff for journal mode.
func lineDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	start := 0
	for start < len(al) && start < len(bl) && al[start] == bl[start] {
		start++
	}
	ea, eb := len(al)-1, len(bl)-1
	for ea >= start && eb >= start && al[ea] == bl[eb] {
		ea--
		eb--
	}
	var out strings.Builder
	fmt.Fprintf(&out, "@@ line %d @@\n", start+1)
	for i := start; i <= ea; i++ {
		out.WriteString("-" + al[i] + "\n")
	}
	for i := start; i <= eb; i++ {
		out.WriteString("+" + bl[i] + "\n")
	}
	return out.String()
}
