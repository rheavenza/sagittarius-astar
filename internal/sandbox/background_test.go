package sandbox

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRingBufferKeepsTailAndCountsDropped(t *testing.T) {
	r := newRingBuffer(10)
	r.Write([]byte("hello "))
	if d, total, dropped := r.since(0); string(d) != "hello " || total != 6 || dropped != 0 {
		t.Fatalf("since(0) = %q %d %d", d, total, dropped)
	}
	r.Write([]byte("world, again"))
	d, total, dropped := r.since(6)
	if total != 18 || string(d) != "rld, again" || dropped != 2 {
		t.Fatalf("since(6) = %q total=%d dropped=%d", d, total, dropped)
	}
	if d, _, _ := r.since(18); len(d) != 0 {
		t.Fatalf("nothing new expected, got %q", d)
	}
	r.Write([]byte(strings.Repeat("x", 25)))
	if d, _, _ := r.since(0); string(d) != strings.Repeat("x", 10) {
		t.Fatalf("a write larger than the buffer keeps its tail, got %q", d)
	}
}

func TestBackgroundProcessOutputAndKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	p, err := StartBackground("echo one; sleep 0.2; echo two; sleep 60 & wait", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.WaitForOutput(context.Background(), 2*time.Second)
	time.Sleep(400 * time.Millisecond)
	if out := p.NewOutput(1024); !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Fatalf("output = %q", out)
	}
	if out := p.NewOutput(1024); out != "" {
		t.Fatalf("second read should be empty, got %q", out)
	}
	start := time.Now()
	p.Kill() // also kills the backgrounded sleep in its process group
	if p.Running() || time.Since(start) > 5*time.Second {
		t.Fatalf("kill: running=%v after %v", p.Running(), time.Since(start))
	}
	if !strings.Contains(p.Status(), "signal") && !strings.Contains(p.Status(), "exited") {
		t.Errorf("status = %q", p.Status())
	}
}
