package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// ── applyEdit ────────────────────────────────────────────────────────────────

func TestApplyEditExactAndReplaceAll(t *testing.T) {
	out, note, err := applyEdit("a := 1\nb := 2\n", editOp{Old: "b := 2", New: "b := 3"})
	if err != nil || out != "a := 1\nb := 3\n" || note != "" {
		t.Fatalf("exact: out=%q note=%q err=%v", out, note, err)
	}
	if _, _, err := applyEdit("x x", editOp{Old: "x", New: "y"}); err == nil {
		t.Fatal("ambiguous match must fail without replace_all")
	}
	out, _, err = applyEdit("x x", editOp{Old: "x", New: "y", ReplaceAll: true})
	if err != nil || out != "y y" {
		t.Fatalf("replace_all: %q %v", out, err)
	}
}

func TestApplyEditStripsCopiedLineNumbers(t *testing.T) {
	content := "func f() {\n\treturn 1\n}\n"
	old := "     1\tfunc f() {\n     2\t\treturn 1"
	out, note, err := applyEdit(content, editOp{Old: old, New: "func f() {\n\treturn 2"})
	if err != nil {
		t.Fatal(err)
	}
	if out != "func f() {\n\treturn 2\n}\n" || !strings.Contains(note, "line-number") {
		t.Fatalf("out=%q note=%q", out, note)
	}
}

func TestApplyEditWhitespaceTolerantReindents(t *testing.T) {
	content := "func f() {\n\tif x {\n\t\treturn 1\n\t}\n}\n"
	// Model used 4-space indentation instead of tabs.
	old := "    if x {\n        return 1\n    }"
	repl := "    if x {\n        return 2\n    }"
	out, note, err := applyEdit(content, editOp{Old: old, New: repl})
	if err != nil {
		t.Fatal(err)
	}
	want := "func f() {\n\tif x {\n\t    return 2\n\t}\n}\n"
	if out != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if !strings.Contains(note, "whitespace") {
		t.Errorf("expected whitespace note, got %q", note)
	}
}

func TestApplyEditMissingGivesHint(t *testing.T) {
	_, _, err := applyEdit("one\ntwo\nthree\n", editOp{Old: "two\nTHREE", New: "x"})
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("expected a hint pointing at line 2, got %v", err)
	}
}

func TestMultiEditIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o644)

	call := llm.ToolCall{Name: "multi_edit", Args: map[string]any{
		"path": path,
		"edits": []any{
			map[string]any{"old_content": "alpha", "new_content": "ALPHA"},
			map[string]any{"old_content": "gamma", "new_content": "GAMMA"},
		},
	}}
	_, pw, err := Execute(context.Background(), call)
	if err != ErrWriteConfirmationRequired || pw == nil {
		t.Fatalf("expected pending write, got %v", err)
	}
	if pw.Content != "ALPHA\nbeta\nGAMMA\n" {
		t.Fatalf("content = %q", pw.Content)
	}

	call.Args["edits"] = []any{
		map[string]any{"old_content": "alpha", "new_content": "ALPHA"},
		map[string]any{"old_content": "missing", "new_content": "x"},
	}
	if _, pw, err := Execute(context.Background(), call); pw != nil || err == nil || !strings.Contains(err.Error(), "edit #2") {
		t.Fatalf("a failing edit must reject the whole batch: pw=%v err=%v", pw, err)
	}
}

// ── read_file ────────────────────────────────────────────────────────────────

func TestReadFileNumbersLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	os.WriteFile(path, []byte("package x\n\nfunc A() {}\n"), 0o644)

	out, _, err := Execute(context.Background(), llm.ToolCall{Name: "read_file", Args: map[string]any{"path": path}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "     1\tpackage x\n     2\t\n     3\tfunc A() {}") {
		t.Fatalf("unexpected output:\n%q", out)
	}

	out, _, _ = Execute(context.Background(), llm.ToolCall{Name: "read_file", Args: map[string]any{"path": path, "start_line": float64(3)}})
	if !strings.Contains(out, "lines 3-3 of 3") || !strings.Contains(out, "     3\tfunc A() {}") {
		t.Fatalf("range read:\n%s", out)
	}
}

func TestReadFileTruncatesOnLineBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.txt")
	os.WriteFile(path, []byte(strings.Repeat(strings.Repeat("x", 99)+"\n", 2000)), 0o644)
	out, _, _ := Execute(context.Background(), llm.ToolCall{Name: "read_file", Args: map[string]any{"path": path}})
	if len(out) > maxReadBytes+200 || !strings.Contains(out, "continue with start_line=") {
		t.Fatalf("not truncated properly: %d bytes", len(out))
	}
}

// ── find_files / glob ────────────────────────────────────────────────────────

func TestGlobMatcher(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.go", "internal/tui/model.go", true},
		{"*.go", "README.md", false},
		{"internal/**/*_test.go", "internal/tui/tools_test.go", true},
		{"internal/**/*_test.go", "internal/x_test.go", true},
		{"internal/**/*_test.go", "cmd/x_test.go", false},
		{"*.{ts,tsx}", "src/a.tsx", true},
		{"src/*.ts", "src/deep/a.ts", false},
		{"[mM]akefile", "Makefile", true},
	}
	for _, c := range cases {
		m, err := globMatcher(c.pattern)
		if err != nil {
			t.Fatal(err)
		}
		if got := m(c.path); got != c.want {
			t.Errorf("%s vs %s: got %v want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestFindFilesRespectsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	os.MkdirAll(filepath.Join(root, "gen"), 0o755)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("gen/\n"), 0o644)
	os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg"), 0o644)
	os.WriteFile(filepath.Join(root, "gen", "b.go"), []byte("package gen"), 0o644)

	out, err := findFiles("*.go", root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pkg/a.go") || strings.Contains(out, "gen/b.go") {
		t.Fatalf("gitignore not respected:\n%s", out)
	}
}

// ── post-edit checks ─────────────────────────────────────────────────────────

func TestPostEditCheckJSONAndCustom(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "a.json")
	bad := filepath.Join(dir, "b.json")
	os.WriteFile(good, []byte(`{"ok": true}`), 0o644)
	os.WriteFile(bad, []byte(`{"ok": `), 0o644)

	if got := PostEditCheck(context.Background(), good, nil); !strings.Contains(got, "OK") {
		t.Errorf("valid JSON: %q", got)
	}
	if got := PostEditCheck(context.Background(), bad, nil); !strings.Contains(got, "FAILED") {
		t.Errorf("invalid JSON: %q", got)
	}
	if got := PostEditCheck(context.Background(), bad, map[string]string{".json": "off"}); got != "" {
		t.Errorf("'off' should disable checks, got %q", got)
	}
	custom := map[string]string{".txt": "test -s {file}"}
	txt := filepath.Join(dir, "it's.txt") // quote-safety
	os.WriteFile(txt, []byte("x"), 0o644)
	if got := PostEditCheck(context.Background(), txt, custom); !strings.Contains(got, "OK") {
		t.Errorf("custom check: %q", got)
	}
}

func TestPostEditCheckGoReportsCompileErrors(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/m\n\ngo 1.25\n"), 0o644)
	src := filepath.Join(dir, "a.go")
	os.WriteFile(src, []byte("package m\n\nfunc F() int { return \"no\" }\n"), 0o644)
	got := PostEditCheck(context.Background(), src, nil)
	if !strings.Contains(got, "FAILED") {
		t.Fatalf("expected a compile failure, got %q", got)
	}
}
