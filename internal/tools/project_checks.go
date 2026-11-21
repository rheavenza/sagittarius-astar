package tools

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
)

const (
	// DefaultChecksTimeout bounds each project check unless configured.
	DefaultChecksTimeout = 5 * time.Minute
	// MaxChecksTimeout is the ceiling for checks_timeout_seconds.
	MaxChecksTimeout = 30 * time.Minute

	// Output kept per check: the head shows the first errors, the tail the
	// summary (test runners print "FAIL"/totals last).
	checkOutputHead = 2 * 1024
	checkOutputTail = 3 * 1024
)

// ProjectCheck is a named project check: a shell command run from Root.
type ProjectCheck struct {
	Name    string
	Command string
}

// ProjectChecks is the project's definition of done.
type ProjectChecks struct {
	Root    string
	Checks  []ProjectCheck
	Timeout time.Duration // per check; 0 = DefaultChecksTimeout
}

// Select returns the checks named in names (all if empty), in configured
// order, and an error naming any unknown ones.
func (pc ProjectChecks) Select(names []string) ([]ProjectCheck, error) {
	if len(names) == 0 {
		return pc.Checks, nil
	}
	want := map[string]bool{}
	for _, n := range names {
		want[strings.TrimSpace(n)] = true
	}
	var out []ProjectCheck
	for _, c := range pc.Checks {
		if want[c.Name] {
			out = append(out, c)
			delete(want, c.Name)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for n := range want {
			unknown = append(unknown, n)
		}
		return nil, fmt.Errorf("unknown check(s) %s; configured: %s", strings.Join(unknown, ", "), strings.Join(pc.Names(), ", "))
	}
	return out, nil
}

// Names lists the configured check names.
func (pc ProjectChecks) Names() []string {
	out := make([]string, len(pc.Checks))
	for i, c := range pc.Checks {
		out[i] = c.Name
	}
	return out
}

// CheckNames extracts the "names" argument of a run_checks call.
func CheckNames(call llm.ToolCall) []string {
	var out []string
	switch v := call.Args["names"].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = v
	case string: // some models send a comma-separated string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// RunChecksDefinition describes the run_checks tool for the configured checks.
func RunChecksDefinition(pc ProjectChecks) llm.Tool {
	var list []string
	for _, c := range pc.Checks {
		list = append(list, fmt.Sprintf("%s (`%s`)", c.Name, c.Command))
	}
	return llm.Tool{
		Name: "run_checks",
		Description: "Run the project's configured checks (its definition of done) from the project root and report PASS/FAIL per check with the relevant output. " +
			"Run them before declaring a code change finished, and fix failures. Configured checks: " + strings.Join(list, ", ") + ".",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Checks to run, in configured order. Omit to run all.",
				},
			},
		},
	}
}

// CheckResult is the outcome of one check.
type CheckResult struct {
	Check    ProjectCheck
	Passed   bool
	ExitCode int
	Duration time.Duration
	Output   string
	Note     string // timeout / cancellation
}

// RunProjectChecks runs checks sequentially from pc.Root. All requested
// checks run even if one fails, unless ctx is cancelled.
func RunProjectChecks(ctx context.Context, pc ProjectChecks, checks []ProjectCheck) []CheckResult {
	timeout := pc.Timeout
	if timeout <= 0 {
		timeout = DefaultChecksTimeout
	}
	if timeout > MaxChecksTimeout {
		timeout = MaxChecksTimeout
	}
	var results []CheckResult
	for _, c := range checks {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		res, err := sandbox.Execute(ctx, c.Command, sandbox.Options{
			WorkingDir: pc.Root,
			Timeout:    timeout,
			MaxTimeout: MaxChecksTimeout,
		})
		r := CheckResult{Check: c, Duration: time.Since(start)}
		if err != nil {
			r.ExitCode = -1
			r.Output = err.Error()
		} else {
			r.ExitCode = res.ExitCode
			r.Passed = res.ExitCode == 0
			r.Output = strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
			r.Note = res.Note
		}
		results = append(results, r)
	}
	return results
}

// FormatCheckResults renders results for the model: a summary line, then
// each check's status, with output for failures only.
func FormatCheckResults(results []CheckResult, requested int) string {
	var b strings.Builder
	passed := 0
	for _, r := range results {
		if r.Passed {
			passed++
		}
	}
	switch {
	case len(results) < requested:
		fmt.Fprintf(&b, "Checks interrupted: %d of %d ran, %d passed.\n", len(results), requested, passed)
	case passed == len(results):
		fmt.Fprintf(&b, "All %d checks passed.\n", len(results))
	default:
		fmt.Fprintf(&b, "%d of %d checks FAILED.\n", len(results)-passed, len(results))
	}
	for _, r := range results {
		status := "PASS"
		if !r.Passed {
			status = fmt.Sprintf("FAIL (exit %d)", r.ExitCode)
		}
		fmt.Fprintf(&b, "\n%s %s: %s (%s)\n", checkMark(r.Passed), r.Check.Name, status, r.Duration.Round(100*time.Millisecond))
		fmt.Fprintf(&b, "  $ %s\n", r.Check.Command)
		if r.Note != "" {
			fmt.Fprintf(&b, "  note: %s\n", r.Note)
		}
		if !r.Passed && r.Output != "" {
			b.WriteString(indent(headTail(r.Output, checkOutputHead, checkOutputTail), "  ") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func checkMark(ok bool) string {
	if ok {
		return "✔"
	}
	return "✘"
}

// headTail keeps the first head and last tail bytes of s.
func headTail(s string, head, tail int) string {
	if len(s) <= head+tail {
		return s
	}
	h := TruncateUTF8(s, head)
	t := s[len(s)-tail:]
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return h + fmt.Sprintf("\n…(%d bytes omitted)…\n", len(s)-len(h)-len(t)) + t
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
