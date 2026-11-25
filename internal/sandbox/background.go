package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
)

// Background processes: long-running commands (dev servers, watchers, slow
// builds) that keep running while the agent continues. Their combined
// stdout/stderr goes to a ring buffer, and the caller reads what is new.

// BackgroundBufferBytes is how much recent output a background process keeps.
const BackgroundBufferBytes = 256 * 1024

// killGrace is how long a killed process group gets between SIGTERM and SIGKILL.
const killGrace = 3 * time.Second

// BgProcess is a command running in the background.
type BgProcess struct {
	Command string
	Dir     string
	Started time.Time

	cmd  *exec.Cmd
	out  *ringBuffer
	done chan struct{}

	mu       sync.Mutex
	exitCode int
	exitErr  string
	ended    time.Time
	readTo   int64 // output offset the caller has read up to
}

// StartBackground starts command in the background in its own process group
// (Windows: its own console process tree). It does not support interactive
// sudo password prompts: callers must refuse commands that need one
// (NeedsPassword); the graphical askpass helper works.
func StartBackground(command, dir string) (*BgProcess, error) {
	argv, env := shellArgv(command, "")
	cmd := exec.Command(argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = env
	}
	cmd.Dir = dir
	setProcessGroup(cmd)
	p := &BgProcess{Command: command, Dir: dir, cmd: cmd, out: newRingBuffer(BackgroundBufferBytes), done: make(chan struct{})}
	cmd.Stdout = p.out
	cmd.Stderr = p.out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start background command: %w", err)
	}
	p.Started = time.Now()
	logger.L.Info("sandbox: background command started", "pid", cmd.Process.Pid, "command", command, "dir", dir)
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.ended = time.Now()
		p.exitCode = cmd.ProcessState.ExitCode()
		if err != nil && p.exitCode == -1 {
			p.exitErr = err.Error()
		}
		p.mu.Unlock()
		close(p.done)
		logger.L.Info("sandbox: background command ended", "pid", cmd.Process.Pid, "exit_code", p.exitCode)
	}()
	return p, nil
}

// PID is the process (group leader) id.
func (p *BgProcess) PID() int { return p.cmd.Process.Pid }

// Done is closed when the process has exited.
func (p *BgProcess) Done() <-chan struct{} { return p.done }

// Running reports whether the process is still running.
func (p *BgProcess) Running() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Status is a one-line status, e.g. "running for 2m10s" or "exited with code 1 after 4s".
func (p *BgProcess) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ended.IsZero() {
		return "running for " + time.Since(p.Started).Round(time.Second).String()
	}
	d := p.ended.Sub(p.Started).Round(100 * time.Millisecond)
	if p.exitErr != "" {
		return fmt.Sprintf("ended (%s) after %s", p.exitErr, d)
	}
	return fmt.Sprintf("exited with code %d after %s", p.exitCode, d)
}

// NewOutput returns the output written since the last NewOutput call, at
// most max bytes (the most recent ones), and a note when some was dropped.
func (p *BgProcess) NewOutput(max int) string {
	p.mu.Lock()
	from := p.readTo
	p.mu.Unlock()
	data, total, dropped := p.out.since(from)
	p.mu.Lock()
	p.readTo = total
	p.mu.Unlock()
	note := ""
	if dropped > 0 {
		note = fmt.Sprintf("[%d older bytes were dropped from the buffer]\n", dropped)
	}
	if len(data) > max {
		note += fmt.Sprintf("[showing the last %d of %d new bytes]\n", max, len(data))
		data = data[len(data)-max:]
	}
	return note + strings.ToValidUTF8(string(data), "")
}

// Tail returns up to max bytes of the most recent output, without moving the
// read position.
func (p *BgProcess) Tail(max int) string {
	data, _, _ := p.out.since(0)
	if len(data) > max {
		data = data[len(data)-max:]
	}
	return strings.ToValidUTF8(string(data), "")
}

// Wait waits until the process exits or d passes; it reports whether it exited.
func (p *BgProcess) Wait(d time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(d):
		return false
	}
}

// WaitForOutput waits until there is unread output, the process exits, d
// passes or ctx is cancelled.
func (p *BgProcess) WaitForOutput(ctx context.Context, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		from := p.readTo
		p.mu.Unlock()
		if p.out.total() > from || !p.Running() {
			return
		}
		select {
		case <-p.done:
			return
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Kill stops the process and everything it started: SIGTERM to the group,
// then SIGKILL after a grace period (Windows: taskkill /T /F).
func (p *BgProcess) Kill() {
	if !p.Running() {
		return
	}
	killTree(p.cmd, false)
	if p.Wait(killGrace) {
		return
	}
	killTree(p.cmd, true)
	p.Wait(killGrace)
}

// ringBuffer keeps the last size bytes written and counts all of them.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []byte
	size  int
	count int64 // total bytes ever written
}

func newRingBuffer(size int) *ringBuffer { return &ringBuffer{size: size} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.count += int64(len(p))
	if len(p) >= r.size {
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return len(p), nil
	}
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.size; over > 0 {
		r.buf = append(r.buf[:0], r.buf[over:]...)
	}
	return len(p), nil
}

func (r *ringBuffer) total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// since returns the bytes written after offset from that are still in the
// buffer, the total written, and how many bytes after from were dropped.
func (r *ringBuffer) since(from int64) (data []byte, total, dropped int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := r.count - int64(len(r.buf)) // offset of buf[0]
	if from < start {
		dropped = start - from
		from = start
	}
	if from > r.count {
		from = r.count
	}
	return append([]byte(nil), r.buf[from-start:]...), r.count, dropped
}
