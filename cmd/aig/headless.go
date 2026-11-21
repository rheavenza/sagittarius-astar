package main

// Headless mode: aig -p "prompt" runs one turn without the TUI. The answer
// goes to stdout; tool activity, warnings and denials go to stderr.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/clitorhea/sagittarius-astar.git/internal/agent"
	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/mention"
	"github.com/clitorhea/sagittarius-astar.git/internal/session"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// maxStdinBytes bounds piped input; the context window may bound it further.
const maxStdinBytes = 4 << 20

// exitError makes main exit with code, printing msg (if any) on stderr.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// usageTotals sums the usage of every request of the run.
type usageTotals struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
	// Estimated is true if some request's usage wasn't reported by the API.
	Estimated bool `json:"estimated,omitempty"`
}

// headlessResult is the json output, and the last stream-json event.
type headlessResult struct {
	Type       string      `json:"type,omitempty"`
	Result     string      `json:"result"`
	IsError    bool        `json:"is_error"`
	Error      string      `json:"error,omitempty"`
	SessionID  string      `json:"session_id"`
	Provider   string      `json:"provider"`
	Model      string      `json:"model"`
	Usage      usageTotals `json:"usage"`
	Requests   int         `json:"requests"`
	ToolCalls  int         `json:"tool_calls"`
	Denied     []string    `json:"denied,omitempty"`
	DurationMS int64       `json:"duration_ms"`
}

func runHeadless(_ *cobra.Command, args []string) error {
	format := flagOutputFormat
	if format != "text" && format != "json" && format != "stream-json" {
		return fmt.Errorf("unknown --output-format %q (text | json | stream-json)", format)
	}
	promptText := strings.TrimSpace(strings.Join(args, " "))

	// Piped input is read before setup: consent prompts need a terminal on
	// stdin anyway and are skipped without one.
	piped := ""
	truncatedStdin := false
	if !isTerminal(os.Stdin) {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, maxStdinBytes+1))
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		if len(data) > maxStdinBytes {
			data, truncatedStdin = data[:maxStdinBytes], true
		}
		piped = strings.TrimRight(strings.ToValidUTF8(string(data), "�"), "\n")
	}
	if promptText == "" && strings.TrimSpace(piped) == "" {
		return fmt.Errorf(`aig -p needs a prompt: aig -p "…", or pipe input into it`)
	}

	rt, err := setup()
	if err != nil {
		return err
	}
	defer rt.close()

	// Keep the input well inside the context window.
	if limit := rt.agent.ContextWindow() * 2; len(piped) > limit { // ~4 bytes/token: half the window
		piped, truncatedStdin = tools.TruncateUTF8(piped, limit), true
	}
	if truncatedStdin {
		fmt.Fprintf(os.Stderr, "aig: stdin truncated to %d bytes to fit the context window\n", len(piped))
		piped += "\n…[stdin truncated]"
	}

	cwd, _ := os.Getwd()
	content, atts := mention.Expand(promptText, cwd)
	for _, a := range atts {
		switch {
		case a.Err != nil:
			fmt.Fprintln(os.Stderr, "aig: "+a.Label())
		case !flagQuiet && isTerminal(os.Stderr):
			fmt.Fprintln(os.Stderr, "📎 "+a.Label())
		}
	}
	switch {
	case piped == "":
	case promptText == "":
		content = piped
	default:
		content += "\n\n<stdin>\n" + piped + "\n</stdin>"
	}

	sess := rt.session
	if !sess.HasContent() || sess.Name == "" || sess.Name == "Untitled" {
		sess.Name = sessionName(firstNonEmpty(promptText, piped))
	}
	history := append(append([]llm.Message(nil), sess.History...), llm.Message{Role: llm.RoleUser, Content: content})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out := newHeadlessOutput(format, flagQuiet)
	res := headlessResult{SessionID: sess.ID, Provider: string(rt.cfg.Provider), Model: rt.cfg.Model}
	out.event(map[string]any{
		"type": "init", "session_id": sess.ID, "provider": res.Provider, "model": res.Model,
		"permission_mode": permissionModeName(rt.agent),
	})

	start := time.Now()
	finished, aborted := false, false
	var runErr error
	for ev := range rt.agent.Run(ctx, history) {
		switch e := ev.(type) {
		case agent.TokenEvent:
			if !e.Reasoning {
				out.event(map[string]any{"type": "text", "text": e.Text})
			}
		case agent.MessageEvent:
			history = append(history, e.Msg)
			if e.Msg.Role == llm.RoleAssistant && len(e.Msg.ToolCalls) == 0 {
				res.Result, finished = e.Msg.Content, true
			}
		case agent.HistoryEvent:
			history = append([]llm.Message(nil), e.History...)
		case agent.ApprovalEvent:
			res.Denied = append(res.Denied, e.Req.Title)
			e.Req.DenyWithReason(headlessDenial(rt.agent, e.Req))
			out.denied(e.Req)
		case agent.ToolStartEvent:
			// Nobody can type into a running command: give it EOF.
			if c, ok := e.Stdin.(io.Closer); ok {
				_ = c.Close()
			}
			res.ToolCalls++
			out.toolStart(e)
		case agent.ToolResultEvent:
			out.toolResult(e)
		case agent.NoticeEvent:
			out.notice(e)
		case agent.UsageEvent:
			res.Requests++
			res.Usage.InputTokens += e.Usage.InputTokens
			res.Usage.OutputTokens += e.Usage.OutputTokens
			res.Usage.CachedTokens += e.Usage.CachedTokens
			res.Usage.Estimated = res.Usage.Estimated || !e.Reported
			out.event(map[string]any{"type": "usage", "input_tokens": e.Usage.InputTokens, "output_tokens": e.Usage.OutputTokens,
				"cached_tokens": e.Usage.CachedTokens, "reported": e.Reported, "context_window": e.Window})
		case agent.DoneEvent:
			runErr, aborted = e.Err, e.Aborted
		}
	}
	res.DurationMS = time.Since(start).Milliseconds()

	sess.History = history
	sess.UpdatedAt = time.Now()
	if err := session.Save(sess); err != nil {
		logger.L.Error("failed to save headless session", "id", sess.ID, "error", err)
		fmt.Fprintf(os.Stderr, "aig: session not saved: %v\n", err)
	}

	code := 0
	switch {
	case aborted:
		res.Error, code = "interrupted", 130
	case runErr != nil:
		res.Error, code = runErr.Error(), 1
	case !finished:
		res.Error, code = "stopped before the model finished its answer (continue with: aig -r -p \"continue\")", 1
	}
	res.IsError = code != 0
	out.finish(res)
	if code != 0 {
		msg := res.Error
		if format != "text" {
			msg = "" // already in the JSON
		}
		return &exitError{code: code, msg: msg}
	}
	return nil
}

// headlessDenial is what the model is told when a call needs approval.
func headlessDenial(a *agent.Agent, req *agent.ApprovalRequest) string {
	return fmt.Sprintf("Not run: aig is running headless (aig -p) in %s permission mode, so this call needed approval (%s) "+
		"and was denied, as nobody can be asked. Do not retry it. Do what you can without it, then state exactly what is still needed "+
		"(the user can rerun with --permission-mode accept-edits to allow file edits, or auto to also allow commands).",
		permissionModeName(a), req.Title)
}

func permissionModeName(a *agent.Agent) string {
	if a.PlanMode() {
		return "plan"
	}
	return a.Mode().String()
}

// sessionName derives a session name from the prompt's first line.
func sessionName(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 60 {
		s = tools.TruncateUTF8(s, 57) + "…"
	}
	return "-p " + s
}

// headlessOutput writes the answer and events in the chosen format.
type headlessOutput struct {
	format   string
	progress bool // tool activity on stderr
	color    bool
	enc      *json.Encoder
}

func newHeadlessOutput(format string, quiet bool) *headlessOutput {
	tty := isTerminal(os.Stderr)
	return &headlessOutput{
		format:   format,
		progress: !quiet && tty,
		color:    tty && os.Getenv("NO_COLOR") == "",
		enc:      json.NewEncoder(os.Stdout),
	}
}

// event writes one stream-json line.
func (o *headlessOutput) event(v any) {
	if o.format == "stream-json" {
		_ = o.enc.Encode(v)
	}
}

func (o *headlessOutput) dim(s string) string {
	if !o.color {
		return s
	}
	return "\x1b[2m" + s + "\x1b[0m"
}

func (o *headlessOutput) toolStart(e agent.ToolStartEvent) {
	o.event(map[string]any{"type": "tool_start", "id": e.Call.ID, "name": e.Call.Name, "args": e.Call.Args, "command": e.Command, "auto": e.Auto})
	if !o.progress {
		return
	}
	line := agent.DescribeCall(e.Call)
	if e.Command != "" {
		line = e.Call.Name + " " + e.Command
	}
	fmt.Fprintln(os.Stderr, o.dim("› "+tools.TruncateUTF8(oneLine(line), 200)))
}

func (o *headlessOutput) toolResult(e agent.ToolResultEvent) {
	ev := map[string]any{"type": "tool_result", "id": e.Call.ID, "name": e.Call.Name, "result": e.Result, "denied": e.Denied}
	if e.Err != nil {
		ev["error"] = e.Err.Error()
	}
	o.event(ev)
	if o.progress && e.Err != nil {
		fmt.Fprintln(os.Stderr, o.dim("  ✗ "+tools.TruncateUTF8(oneLine(e.Err.Error()), 200)))
	}
}

func (o *headlessOutput) denied(req *agent.ApprovalRequest) {
	o.event(map[string]any{"type": "denied", "name": req.Call.Name, "title": req.Title, "command": req.Command})
	what := req.Title
	if req.Command != "" {
		what += ": " + oneLine(req.Command)
	}
	if !flagQuiet {
		fmt.Fprintln(os.Stderr, "aig: denied (needs approval): "+tools.TruncateUTF8(what, 200))
	}
}

func (o *headlessOutput) notice(e agent.NoticeEvent) {
	o.event(map[string]any{"type": "notice", "level": [...]string{"info", "warn", "error"}[e.Level], "text": e.Text})
	if e.Level == agent.NoticeInfo && !o.progress {
		return
	}
	if e.Level == agent.NoticeInfo {
		fmt.Fprintln(os.Stderr, o.dim(e.Text))
		return
	}
	fmt.Fprintln(os.Stderr, "aig: "+e.Text)
}

func (o *headlessOutput) finish(res headlessResult) {
	switch o.format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	case "stream-json":
		res.Type = "result"
		_ = o.enc.Encode(res)
	default:
		if res.Result != "" {
			fmt.Fprint(os.Stdout, res.Result)
			if !strings.HasSuffix(res.Result, "\n") {
				fmt.Fprintln(os.Stdout)
			}
		}
	}
	if o.progress {
		est := ""
		if res.Usage.Estimated {
			est = "~"
		}
		fmt.Fprintln(os.Stderr, o.dim(fmt.Sprintf("session %s · %s%s in / %s out · %d tool calls · %.1fs",
			res.SessionID, est, humanCount(res.Usage.InputTokens), humanCount(res.Usage.OutputTokens), res.ToolCalls, float64(res.DurationMS)/1000)))
	}
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func humanCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprint(n)
	}
}

// exitCode handles an *exitError from Execute.
func exitCode(err error) (int, bool) {
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "aig: "+ee.msg)
		}
		return ee.code, true
	}
	return 0, false
}
