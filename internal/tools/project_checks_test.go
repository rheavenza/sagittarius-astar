package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

func TestProjectChecksSelect(t *testing.T) {
	pc := ProjectChecks{Checks: []ProjectCheck{{"build", "b"}, {"test", "t"}, {"lint", "l"}}}
	got, err := pc.Select([]string{"lint", "build"})
	if err != nil || len(got) != 2 || got[0].Name != "build" || got[1].Name != "lint" {
		t.Fatalf("Select = %v, %v (want configured order)", got, err)
	}
	if all, _ := pc.Select(nil); len(all) != 3 {
		t.Errorf("Select(nil) = %v", all)
	}
	if _, err := pc.Select([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "build, test, lint") {
		t.Errorf("unknown check error = %v", err)
	}
}

func TestCheckNamesArgShapes(t *testing.T) {
	for _, args := range []map[string]any{
		{"names": []any{"a", "b"}},
		{"names": "a, b"},
	} {
		if got := CheckNames(llm.ToolCall{Args: args}); strings.Join(got, "|") != "a|b" {
			t.Errorf("%v → %v", args, got)
		}
	}
	if got := CheckNames(llm.ToolCall{Args: map[string]any{}}); len(got) != 0 {
		t.Errorf("empty → %v", got)
	}
}

func TestRunProjectChecksReportsPassAndFail(t *testing.T) {
	dir := t.TempDir()
	pc := ProjectChecks{Root: dir, Checks: []ProjectCheck{
		{"where", "pwd"},
		{"broken", "echo boom-output >&2; exit 3"},
	}}
	results := RunProjectChecks(context.Background(), pc, pc.Checks)
	if len(results) != 2 || !results[0].Passed || results[1].Passed || results[1].ExitCode != 3 {
		t.Fatalf("results = %+v", results)
	}
	out := FormatCheckResults(results, 2)
	for _, want := range []string{"1 of 2 checks FAILED", "✔ where: PASS", "✘ broken: FAIL (exit 3)", "boom-output"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Output of passing checks is not sent to the model.
	if strings.Contains(out, dir) {
		t.Errorf("passing check output leaked:\n%s", out)
	}
}

func TestHeadTailKeepsBothEnds(t *testing.T) {
	s := "HEAD" + strings.Repeat("x", 10000) + "TAIL"
	got := headTail(s, 100, 100)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "bytes omitted") {
		t.Errorf("headTail = %q", got)
	}
}
