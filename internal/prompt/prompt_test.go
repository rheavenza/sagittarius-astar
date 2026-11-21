package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectStackAndRender(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.25\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n"), 0o644)

	e := DetectEnv(dir)
	out := e.Render()
	for _, want := range []string{"Working directory: " + dir, "Go (module example.com/x)", "Makefile", "Date: "} {
		if !strings.Contains(out, want) {
			t.Errorf("env block missing %q:\n%s", want, out)
		}
	}
}

func TestFindAndRenderInstructions(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	os.MkdirAll(filepath.Join(root, ".aig"), 0o755)
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root rules"), 0o644)
	os.WriteFile(filepath.Join(root, ".aig", "instructions.md"), []byte("aig rules"), 0o644)
	os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("sub rules"), 0o644)

	paths := FindInstructionFiles(sub, root)
	if len(paths) != 3 || !strings.HasSuffix(paths[0], filepath.Join("sub", "CLAUDE.md")) {
		t.Fatalf("unexpected files (cwd first): %v", paths)
	}
	// cwd == gitRoot must not duplicate.
	if got := FindInstructionFiles(root, root); len(got) != 2 {
		t.Fatalf("expected dedup, got %v", got)
	}

	var files []InstructionFile
	for _, p := range paths {
		f, err := LoadInstructionFile(p, false)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	out := RenderInstructions(files)
	for _, want := range []string{"# Project instructions", "root rules", "aig rules", "sub rules"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestLargeInstructionFileTruncated(t *testing.T) {
	p := filepath.Join(t.TempDir(), "AGENTS.md")
	os.WriteFile(p, []byte(strings.Repeat("é", maxInstructionFileBytes)), 0o644)
	f, err := LoadInstructionFile(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Content) > maxInstructionFileBytes+20 || !strings.HasSuffix(f.Content, "(truncated)") {
		t.Fatalf("not truncated: %d bytes", len(f.Content))
	}
}

func TestRenderChecks(t *testing.T) {
	if RenderChecks("x", nil) != "" {
		t.Error("no checks should render nothing")
	}
	s := RenderChecks("/p/.aig/config.json", []Check{{"test", "go test ./..."}})
	for _, want := range []string{"# Definition of done", "run_checks", "- test: `go test ./...`", "/p/.aig/config.json"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}
