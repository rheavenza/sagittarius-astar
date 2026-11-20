//go:build !windows

package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTimeoutKillsBackgroundChildrenAndKeepsOutput(t *testing.T) {
	start := time.Now()
	// The background sleep holds stdout open; before the process-group fix
	// this blocked Run() for the full 30s.
	res, err := Execute(context.Background(), "echo started; sleep 30 & sleep 30", Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("timeout should not be an error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("command took %s; process group was not killed", elapsed)
	}
	if res.ExitCode != -1 || !strings.Contains(res.Note, "timed out") {
		t.Errorf("unexpected result: exit=%d note=%q", res.ExitCode, res.Note)
	}
	if !strings.Contains(res.Stdout, "started") {
		t.Errorf("partial output lost: %q", res.Stdout)
	}
}

func TestCancelReportsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	res, err := Execute(ctx, "sleep 10")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Note, "cancelled") {
		t.Errorf("expected cancelled note, got %q", res.Note)
	}
}

func TestOutputIsCapped(t *testing.T) {
	res, err := Execute(context.Background(), "head -c 3000000 /dev/zero | tr '\\0' 'a'", Options{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > MaxCaptureBytes+200 {
		t.Fatalf("stdout not capped: %d bytes", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "output truncated") {
		t.Error("missing truncation marker")
	}
	if c := res.Combined(); len(c) > 2*MaxCombinedBytes+500 {
		t.Errorf("Combined() too large for the LLM: %d bytes", len(c))
	}
}

func TestExitCodePropagates(t *testing.T) {
	res, err := Execute(context.Background(), "echo oops >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Stderr, "oops") {
		t.Fatalf("got exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

func TestRewriteSudo(t *testing.T) {
	cases := map[string]string{
		"sudo dnf update":           "sudo -A dnf update",
		"sudo -A ls && sudo whoami": "sudo -A ls && sudo -A whoami",
		"visudo":                    "visudo",
	}
	for in, want := range cases {
		if got := rewriteSudo(in, "-A"); got != want {
			t.Errorf("rewriteSudo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAskpassRequiresDisplay(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	if AskpassHelper() != "" {
		t.Error("askpass must not be used without a graphical session")
	}
}

func TestOpenInteractiveStdinDoesNotBlockCompletion(t *testing.T) {
	// The caller keeps the pipe open (as the TUI does for Ctrl+I input).
	pr, pw := io.Pipe()
	defer pw.Close()
	start := time.Now()
	res, err := ExecuteWithStdin(context.Background(), "echo hi", pr, Options{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("command that ignores stdin took %s", elapsed)
	}
	if strings.TrimSpace(res.Stdout) != "hi" || res.Note != "" {
		t.Fatalf("stdout=%q note=%q", res.Stdout, res.Note)
	}
}

func TestInteractiveStdinIsDelivered(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprintln(pw, "world")
	}()
	res, err := ExecuteWithStdin(context.Background(), "read x; echo hello $x", pr, Options{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "hello world" {
		t.Fatalf("stdout=%q", res.Stdout)
	}
}
