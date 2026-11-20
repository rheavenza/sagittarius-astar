package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

func TestSplitReasoningBothTags(t *testing.T) {
	content, reasoning, open := SplitReasoning("a<think>r1</think>b<thought>r2</thought>c")
	if content != "abc" || reasoning != "r1r2" || open {
		t.Fatalf("got content=%q reasoning=%q open=%v", content, reasoning, open)
	}
	content, reasoning, open = SplitReasoning("x<thought>partial")
	if content != "x" || reasoning != "partial" || !open {
		t.Fatalf("unclosed block: content=%q reasoning=%q open=%v", content, reasoning, open)
	}
}

func TestSimpleDiff(t *testing.T) {
	d := simpleDiff("a\nb\nc", "a\nB\nc")
	if !strings.Contains(d, "- b") || !strings.Contains(d, "+ B") {
		t.Fatalf("unexpected diff:\n%s", d)
	}
	if simpleDiff("same", "same") != "(no changes)" {
		t.Fatal("identical content should report no changes")
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "héllo" // é is 2 bytes at index 1-2
	got := truncateUTF8(s, 2)
	if !utf8.ValidString(got) || got != "h" {
		t.Fatalf("got %q", got)
	}
}

func TestCommitWriteRejectsStaleProposal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, pw, _ := ExecuteToolWithWrite(llm.ToolCall{Name: "edit_file", Args: map[string]any{
		"path": path, "old_content": "one", "new_content": "two",
	}})
	if pw == nil {
		t.Fatal("expected a pending write")
	}

	// Someone edits the file after the diff was shown.
	if err := os.WriteFile(path, []byte("one\nnewer line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitWrite(*pw); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("expected ErrStaleWrite, got %v", err)
	}

	// A fresh proposal commits fine.
	_, pw, _ = ExecuteToolWithWrite(llm.ToolCall{Name: "write_file", Args: map[string]any{
		"path": path, "content": "fresh",
	}})
	if err := CommitWrite(*pw); err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "fresh" {
		t.Fatalf("file content = %q", b)
	}
}

func TestNativeToolNeedsApproval(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "x.go")

	cases := []struct {
		call llm.ToolCall
		want bool
	}{
		{llm.ToolCall{Name: "read_file", Args: map[string]any{"path": inside}}, false},
		{llm.ToolCall{Name: "read_file", Args: map[string]any{"path": "/etc/passwd"}}, true},
		{llm.ToolCall{Name: "read_file", Args: map[string]any{"path": filepath.Join(root, "..", "escape")}}, true},
		{llm.ToolCall{Name: "list_directory", Args: map[string]any{}}, false}, // "." == cwd, resolved below
		{llm.ToolCall{Name: "web_fetch", Args: map[string]any{"url": "https://example.com"}}, true},
		{llm.ToolCall{Name: "web_search", Args: map[string]any{"query": "go"}}, false},
	}
	t.Chdir(root)
	for _, c := range cases {
		got, _ := NativeToolNeedsApproval(c.call, root)
		if got != c.want {
			t.Errorf("%s %v: got %v want %v", c.call.Name, c.call.Args, got, c.want)
		}
	}
}

func TestSkippedToolResultsAnswerEveryCall(t *testing.T) {
	calls := []llm.ToolCall{{ID: "a", Name: "x"}, {ID: "b", Name: "y"}}
	got := skippedToolResults(calls, "skipped")
	if len(got) != 2 || got[1].ToolCallID != "b" || got[1].Role != llm.RoleTool {
		t.Fatalf("unexpected results: %+v", got)
	}
}

func TestSearchRegexSkipsIgnoredDirsAndBinaries(t *testing.T) {
	root := t.TempDir()
	must := func(p, data string) {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("src/main.go", "needle here")
	must(".git/objects/ab", "needle in git")
	must("node_modules/pkg/index.js", "needle in deps")
	must("bin/blob", "needle\x00binary")

	out, _, err := ExecuteToolWithWrite(llm.ToolCall{Name: "search_regex", Args: map[string]any{"pattern": "needle", "dir": root}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "main.go") {
		t.Fatalf("expected match in src/main.go:\n%s", out)
	}
	for _, bad := range []string{".git", "node_modules", "blob"} {
		if strings.Contains(out, bad) {
			t.Errorf("search should skip %s:\n%s", bad, out)
		}
	}
}

func TestBuildModelItems(t *testing.T) {
	known := [][2]string{{"meta/llama-3.3-70b-instruct", "Llama 3.3 70B"}}
	live := []string{"meta/llama-3.3-70b-instruct", "moonshotai/kimi-k3", "google/gemma-4-31b-it"}

	all := buildModelItems(known, live, "", "moonshotai/kimi-k3")
	if len(all) != 3 || all[0].ID != "meta/llama-3.3-70b-instruct" {
		t.Fatalf("expected known first and deduplicated: %+v", all)
	}
	if !strings.Contains(all[1].Label, "(active)") {
		t.Errorf("active model not marked: %+v", all[1])
	}

	filtered := buildModelItems(known, live, "GEMMA", "")
	if len(filtered) != 1 || filtered[0].ID != "google/gemma-4-31b-it" {
		t.Fatalf("filter failed: %+v", filtered)
	}
}
