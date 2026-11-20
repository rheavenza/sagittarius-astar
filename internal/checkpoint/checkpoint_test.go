package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func gitRepoFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	write(t, filepath.Join(dir, "tracked.txt"), "v1\n")
	write(t, filepath.Join(dir, "gone.txt"), "will be deleted\n")
	write(t, filepath.Join(dir, ".gitignore"), "ignored/\n")
	run("add", ".")
	run("commit", "-qm", "init")
	// User has a staged change and an untracked file before the agent runs.
	write(t, filepath.Join(dir, "tracked.txt"), "v2-user\n")
	run("add", "tracked.txt")
	write(t, filepath.Join(dir, "notes.txt"), "user notes\n")
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestGitUndoRedoRestoresEverythingAndKeepsIndex(t *testing.T) {
	dir := gitRepoFixture(t)
	statusBefore := gitOut(t, dir, "status", "--porcelain")
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")

	m := New(dir, "sess-1")
	if m.Mode() != "git" {
		t.Fatalf("mode = %s", m.Mode())
	}
	if err := m.Checkpoint("refactor things"); err != nil {
		t.Fatal(err)
	}

	// The "agent" edits, creates, deletes, and touches an ignored file.
	write(t, filepath.Join(dir, "tracked.txt"), "agent edit\n")
	write(t, filepath.Join(dir, "notes.txt"), "agent rewrote notes\n")
	write(t, filepath.Join(dir, "new", "file.go"), "package x\n")
	os.Remove(filepath.Join(dir, "gone.txt"))
	write(t, filepath.Join(dir, "ignored", "cache.bin"), "ignored stuff\n")

	res, err := m.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "tracked.txt")) != "v2-user\n" ||
		read(t, filepath.Join(dir, "notes.txt")) != "user notes\n" ||
		read(t, filepath.Join(dir, "gone.txt")) != "will be deleted\n" {
		t.Fatal("files not restored to the checkpoint")
	}
	if _, err := os.Stat(filepath.Join(dir, "new", "file.go")); !os.IsNotExist(err) {
		t.Error("file created after the checkpoint should be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "new")); !os.IsNotExist(err) {
		t.Error("empty directory left behind")
	}
	if read(t, filepath.Join(dir, "ignored", "cache.bin")) != "ignored stuff\n" {
		t.Error("ignored files must not be touched")
	}
	if !res.Changed() || len(res.Deleted) != 1 || res.Deleted[0] != "new/file.go" {
		t.Errorf("result = %+v", res)
	}

	// The user's index, HEAD and status are exactly as before.
	if got := gitOut(t, dir, "status", "--porcelain"); got != statusBefore {
		t.Errorf("git status changed:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}
	if gitOut(t, dir, "rev-parse", "HEAD") != headBefore {
		t.Error("HEAD moved")
	}

	// Redo brings the agent's changes back.
	if _, err := m.Redo(); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dir, "tracked.txt")) != "agent edit\n" || read(t, filepath.Join(dir, "new", "file.go")) != "package x\n" {
		t.Fatal("redo did not re-apply the changes")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.txt")); !os.IsNotExist(err) {
		t.Error("redo should delete gone.txt again")
	}
}

func TestGitCheckpointsSurviveRestartAndDiff(t *testing.T) {
	dir := gitRepoFixture(t)
	m := New(dir, "sess-2")
	if err := m.Checkpoint("first turn"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "tracked.txt"), "changed by agent\n")
	if err := m.Checkpoint("second turn"); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "b.txt"), "new file\n")

	// A new process for the same session sees the checkpoints.
	m2 := New(dir, "sess-2")
	list := m2.List()
	if len(list) != 2 || list[0].Label != "second turn" || list[1].Seq != 1 {
		t.Fatalf("reloaded checkpoints = %+v", list)
	}

	diff, err := m2.Diff()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"tracked.txt", "+changed by agent", "b.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}

	// Undo twice walks back both turns.
	m2.Undo()
	m2.Undo()
	if read(t, filepath.Join(dir, "tracked.txt")) != "v2-user\n" {
		t.Fatal("double undo did not reach the first checkpoint")
	}
	if _, err := m2.Undo(); err == nil {
		t.Error("third undo should report nothing to undo")
	}
	// Other sessions are isolated.
	if len(New(dir, "other").List()) != 0 {
		t.Error("sessions must not share checkpoints")
	}
}

func TestGitWorkspaceSubdirectory(t *testing.T) {
	dir := gitRepoFixture(t)
	sub := filepath.Join(dir, "pkg")
	write(t, filepath.Join(sub, "a.go"), "package pkg\n")
	write(t, filepath.Join(dir, "outside.txt"), "outside v1\n")

	m := New(sub, "sess-3")
	m.Checkpoint("turn")
	write(t, filepath.Join(sub, "a.go"), "package pkg // edited\n")
	write(t, filepath.Join(dir, "outside.txt"), "outside v2\n")
	m.Undo()
	if read(t, filepath.Join(sub, "a.go")) != "package pkg\n" {
		t.Error("workspace file not restored")
	}
	if read(t, filepath.Join(dir, "outside.txt")) != "outside v2\n" {
		t.Error("undo must not touch files outside the workspace")
	}
}

func TestJournalModeUndoRedo(t *testing.T) {
	dir := t.TempDir() // not a git repo
	existing := filepath.Join(dir, "a.txt")
	created := filepath.Join(dir, "b.txt")
	write(t, existing, "original\n")

	m := New(dir, "s")
	if m.Mode() != "journal" {
		t.Fatalf("mode = %s", m.Mode())
	}
	m.Checkpoint("turn")
	m.RecordWrite(existing, "original\n", true)
	write(t, existing, "edited\n")
	m.RecordWrite(created, "", false)
	write(t, created, "new\n")

	diff, _ := m.Diff()
	if !strings.Contains(diff, "+edited") || !strings.Contains(diff, "(new file)") {
		t.Errorf("journal diff:\n%s", diff)
	}

	if _, err := m.Undo(); err != nil {
		t.Fatal(err)
	}
	if read(t, existing) != "original\n" || read(t, created) != "<missing>" {
		t.Fatal("journal undo failed")
	}
	if _, err := m.Redo(); err != nil {
		t.Fatal(err)
	}
	if read(t, existing) != "edited\n" || read(t, created) != "new\n" {
		t.Fatal("journal redo failed")
	}
}

func TestNewCheckpointClearsRedo(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "s")
	m.Checkpoint("one")
	m.Undo()
	if !m.CanRedo() {
		t.Fatal("expected redo available")
	}
	m.Checkpoint("two")
	if m.CanRedo() {
		t.Fatal("a new checkpoint must clear redo")
	}
}

func TestSymlinkedWorkspaceReportsRelativePaths(t *testing.T) {
	dir := gitRepoFixture(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	m := New(link, "sym")
	m.Checkpoint("turn")
	write(t, filepath.Join(link, "tracked.txt"), "changed\n")
	res, err := m.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Restored) != 1 || res.Restored[0] != "tracked.txt" {
		t.Fatalf("restored = %v, want [tracked.txt] (no ../ paths)", res.Restored)
	}
}
