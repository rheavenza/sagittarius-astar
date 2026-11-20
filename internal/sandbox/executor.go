// Package sandbox provides OS-aware command execution for aig.
// Commands are always gated by explicit user confirmation in the TUI.
//
// NOTE: despite the package name this is NOT an isolation boundary — commands
// run as the current user with full access. Isolation (bubblewrap/Landlock/
// containers) is tracked as a future feature.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
)

// DefaultTimeout is the maximum wall-clock time a sandboxed command may run.
// Commands that exceed this are killed automatically.
const DefaultTimeout = 30 * time.Second

// MaxTimeout is the ceiling a caller may request for a single command.
const MaxTimeout = 120 * time.Second

// MaxCaptureBytes caps how much stdout (and, separately, stderr) is kept in
// memory per command. Output beyond this is counted but discarded.
const MaxCaptureBytes = 1 << 20 // 1 MiB

// MaxCombinedBytes caps each stream in Combined(), i.e. what is fed to the LLM.
// Head and tail are kept; the middle is elided.
const MaxCombinedBytes = 32 * 1024

// waitDelay bounds how long Wait blocks for I/O after the process is killed
// (e.g. an orphaned grandchild still holding the stdout pipe).
const waitDelay = 2 * time.Second

var (
	reSudo       = regexp.MustCompile(`\bsudo\b`)
	reSudoPrompt = regexp.MustCompile(`(?m)^\[sudo\] password for .*: \n?`)
)

// Result holds the combined output from an executed command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// Note is an optional annotation such as "[killed: timed out after 30s]".
	Note string
}

// Combined returns stdout and stderr as a single annotated string suitable
// for injection into LLM conversation history. Each stream is truncated to
// MaxCombinedBytes (head + tail).
func (r Result) Combined() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "exit_code: %d\n", r.ExitCode)
	if r.Note != "" {
		sb.WriteString(r.Note + "\n")
	}
	if r.Stdout != "" {
		sb.WriteString("--- stdout ---\n")
		sb.WriteString(headTail(r.Stdout, MaxCombinedBytes))
	}
	if r.Stderr != "" {
		sb.WriteString("\n--- stderr ---\n")
		sb.WriteString(headTail(r.Stderr, MaxCombinedBytes))
	}
	return strings.TrimSpace(sb.String())
}

// headTail keeps the first and last max/2 bytes of s.
func headTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	half := max / 2
	head := strings.ToValidUTF8(s[:half], "")
	tail := strings.ToValidUTF8(s[len(s)-half:], "")
	return fmt.Sprintf("%s\n... [%d bytes elided] ...\n%s", head, len(s)-2*half, tail)
}

// cappedBuffer is an io.Writer that keeps at most limit bytes and silently
// counts (but drops) the rest, so a runaway command can't exhaust memory.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
			c.dropped += len(p) - room
		}
	} else {
		c.dropped += len(p)
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.dropped == 0 {
		return c.buf.String()
	}
	return c.buf.String() + fmt.Sprintf("\n... [output truncated: %d more bytes discarded]", c.dropped)
}

// Options configures a single Execute call.
type Options struct {
	// WorkingDir sets the command's working directory.
	// An empty string inherits the process working directory.
	WorkingDir string

	// Timeout overrides DefaultTimeout for this call.
	// Values ≤ 0 use DefaultTimeout; values > MaxTimeout are clamped to MaxTimeout.
	Timeout time.Duration

	// SudoPassword is the password to use for sudo commands (fallback when no
	// graphical askpass helper is available).
	SudoPassword string
}

// AskpassHelper returns the path of a graphical sudo askpass helper if one is
// usable in this session, or "" otherwise. $SUDO_ASKPASS wins; otherwise KDE's
// ksshaskpass is used when a Wayland/X11 display is present.
func AskpassHelper() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		return ""
	}
	candidates := []string{os.Getenv("SUDO_ASKPASS"), "/usr/bin/ksshaskpass"}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return c
		}
	}
	return ""
}

// rewriteSudo inserts flag (-A or -S) after every bare `sudo` token, without
// duplicating it if already present.
func rewriteSudo(command, flag string) string {
	out := reSudo.ReplaceAllString(command, "sudo "+flag)
	return strings.ReplaceAll(out, "sudo "+flag+" "+flag, "sudo "+flag)
}

// Execute runs the given command string using the platform-appropriate shell.
// On Linux/macOS it uses /bin/bash -c; on Windows it uses powershell.exe -Command.
//
// A hard timeout is applied automatically. If the parent context is already
// shorter, that deadline wins.
func Execute(ctx context.Context, command string, opts ...Options) (*Result, error) {
	var stdinBuf bytes.Buffer
	if len(opts) > 0 && opts[0].SudoPassword != "" && AskpassHelper() == "" {
		stdinBuf.WriteString(opts[0].SudoPassword + "\n")
	}
	return ExecuteWithStdin(ctx, command, &stdinBuf, opts...)
}

// ExecuteWithStdin is like Execute but accepts a custom io.Reader for stdin.
// Pass an io.Pipe reader to allow the TUI to inject lines while the process runs.
//
// A timed-out or cancelled command is not an error: the partial output is
// returned with ExitCode -1 and a Note describing why it was killed.
func ExecuteWithStdin(ctx context.Context, command string, stdin io.Reader, opts ...Options) (*Result, error) {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	timeout := opt.Timeout
	switch {
	case timeout <= 0:
		timeout = DefaultTimeout
	case timeout > MaxTimeout:
		timeout = MaxTimeout
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := Shell()
	logger.L.Info("sandbox: executing command",
		"shell", shell,
		"os", runtime.GOOS,
		"command", command,
		"working_dir", opt.WorkingDir,
		"timeout", timeout,
	)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.CommandContext(timeoutCtx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", command)
	default: // linux, darwin, etc.
		if reSudo.MatchString(command) {
			if helper := AskpassHelper(); helper != "" {
				// Graphical askpass: sudo pops a KDE dialog per command; aig never
				// sees the password.
				command = rewriteSudo(command, "-A")
				cmd = exec.CommandContext(timeoutCtx, "/bin/bash", "-c", command)
				cmd.Env = append(os.Environ(), "SUDO_ASKPASS="+helper)
			} else if opt.SudoPassword != "" {
				command = rewriteSudo(command, "-S")
			}
		}
		if cmd == nil {
			cmd = exec.CommandContext(timeoutCtx, "/bin/bash", "-c", command)
		}
		cmd.Stdin = stdin
	}

	// Run in its own process group and kill the whole group on timeout/cancel,
	// so background children can't outlive the command or hang Wait().
	configureProcessGroup(cmd)
	cmd.WaitDelay = waitDelay

	if opt.WorkingDir != "" {
		cmd.Dir = opt.WorkingDir
	}

	stdout := &cappedBuffer{limit: MaxCaptureBytes}
	stderr := &cappedBuffer{limit: MaxCaptureBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()

	result := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}

	if opt.SudoPassword != "" {
		result.Stderr = reSudoPrompt.ReplaceAllString(result.Stderr, "")
	}

	// Killed by our own deadline or by the caller (Ctrl+C): keep partial output.
	if ctxErr := timeoutCtx.Err(); ctxErr != nil && (err != nil || cmd.ProcessState == nil) {
		result.ExitCode = -1
		if errors.Is(ctxErr, context.DeadlineExceeded) && ctx.Err() == nil {
			result.Note = fmt.Sprintf("[killed: timed out after %s]", timeout)
			logger.L.Warn("sandbox: command timed out", "timeout", timeout)
		} else {
			result.Note = "[killed: cancelled by user]"
			logger.L.Warn("sandbox: command cancelled")
		}
		return result, nil
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			logger.L.Warn("sandbox: command exited non-zero",
				"exit_code", result.ExitCode,
				"stderr_len", len(result.Stderr),
			)
			return result, nil
		}
		if errors.Is(err, exec.ErrWaitDelay) {
			// The command itself finished; a leftover child kept the pipes open.
			result.Note = "[note: background process kept output open; it was detached]"
			return result, nil
		}
		logger.L.Error("sandbox: failed to start command", "error", err)
		return result, fmt.Errorf("executor: failed to start command: %w", err)
	}

	logger.L.Debug("sandbox: command succeeded",
		"stdout_len", len(result.Stdout),
		"stderr_len", len(result.Stderr),
	)
	return result, nil
}

// Shell returns the shell binary path used on the current OS.
func Shell() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe"
	}
	return "/bin/bash"
}
