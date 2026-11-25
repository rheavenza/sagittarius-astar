package agent

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
)

// Background processes started with run_command(background: true): the
// agent keeps them by id ("proc-1"), the model reads their output with
// read_process_output and stops them with kill_process, and Close kills
// whatever is left when aig exits.

const (
	maxBackgroundProcs = 8
	// startupWait is how long run_command waits for a background command's
	// first output (or its exit), so immediate failures are reported at once.
	startupWait = 2 * time.Second
	// procOutputMax bounds one read of a process's output.
	procOutputMax = 16 * 1024
	maxProcWait   = 60
)

type procTable struct {
	mu    sync.Mutex
	seq   int
	procs map[string]*sandbox.BgProcess
}

func (pt *procTable) add(p *sandbox.BgProcess) string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.procs == nil {
		pt.procs = map[string]*sandbox.BgProcess{}
	}
	pt.seq++
	id := fmt.Sprintf("proc-%d", pt.seq)
	pt.procs[id] = p
	return id
}

func (pt *procTable) get(id string) (*sandbox.BgProcess, bool) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	p, ok := pt.procs[id]
	return p, ok
}

func (pt *procTable) running() int {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	n := 0
	for _, p := range pt.procs {
		if p.Running() {
			n++
		}
	}
	return n
}

func (pt *procTable) any() bool {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return pt.seq > 0
}

// ids returns process ids in start order.
func (pt *procTable) ids() []string {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	ids := make([]string, 0, len(pt.procs))
	for id := range pt.procs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return procNum(ids[i]) < procNum(ids[j]) })
	return ids
}

func procNum(id string) int {
	var n int
	_, _ = fmt.Sscanf(id, "proc-%d", &n)
	return n
}

// RunningProcesses returns how many background processes are running.
func (a *Agent) RunningProcesses() int { return a.procs.running() }

// Close stops every background process the agent started. Call it when
// the front-end exits.
func (a *Agent) Close() {
	var wg sync.WaitGroup
	for _, id := range a.procs.ids() {
		if p, ok := a.procs.get(id); ok && p.Running() {
			wg.Add(1)
			go func() { defer wg.Done(); p.Kill() }()
		}
	}
	wg.Wait()
}

// startBackground runs an approved run_command in the background.
func (t *turn) startBackground(call llm.ToolCall, command string) outcome {
	if n := t.a.procs.running(); n >= maxBackgroundProcs {
		return outcome{err: fmt.Errorf("run_command: %d background processes are already running; stop one with kill_process first", n)}
	}
	dir, _ := call.Args["working_dir"].(string)
	if dir == "" {
		dir = t.a.workspaceRoot
	}
	p, err := sandbox.StartBackground(command, dir)
	if err != nil {
		return outcome{err: err}
	}
	id := t.a.procs.add(p)

	// Give it a moment, so a command that fails at once says so now.
	deadline := time.Now().Add(startupWait)
	for time.Now().Before(deadline) && p.Running() {
		time.Sleep(100 * time.Millisecond)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Started %s in the background (pid %d): %s\nstatus: %s\n", id, p.PID(), command, p.Status())
	if out := p.NewOutput(procOutputMax); strings.TrimSpace(out) != "" {
		b.WriteString("--- output so far ---\n" + out + "\n")
	} else {
		b.WriteString("(no output yet)\n")
	}
	if p.Running() {
		fmt.Fprintf(&b, "Read new output with read_process_output(%q) (wait_seconds waits for more), stop it with kill_process(%q). It is stopped when aig exits.", id, id)
	}
	return outcome{result: strings.TrimRight(b.String(), "\n")}
}

func readProcessOutputDefinition() llm.Tool {
	return llm.Tool{
		Name: "read_process_output",
		Description: "Read the output a background process (started with run_command background: true) wrote since your last read, and its status. " +
			"wait_seconds waits for new output (or the exit) first, e.g. for a server to start. pattern keeps only matching lines. " +
			"Without id, lists all background processes.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":           map[string]any{"type": "string", "description": "Process id, like \"proc-1\"."},
				"wait_seconds": map[string]any{"type": "integer", "description": fmt.Sprintf("Wait up to this long (0-%d) for new output (with pattern: for a matching line) or the exit.", maxProcWait)},
				"pattern":      map[string]any{"type": "string", "description": "Optional regular expression: return only matching lines of the new output."},
			},
		},
	}
}

func killProcessDefinition() llm.Tool {
	return llm.Tool{
		Name:        "kill_process",
		Description: "Stop a background process started with run_command (background: true), including the processes it started. Returns its last output.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Process id, like \"proc-1\"."},
			},
			"required": []string{"id"},
		},
	}
}

// readProcessOutput runs read_process_output.
func (t *turn) readProcessOutput(ctx context.Context, call llm.ToolCall) outcome {
	t.emit(ToolStartEvent{Call: call, Auto: true})
	id, _ := call.Args["id"].(string)
	id = strings.TrimSpace(id)
	if id == "" {
		ids := t.a.procs.ids()
		if len(ids) == 0 {
			return outcome{result: "No background processes."}
		}
		var b strings.Builder
		for _, id := range ids {
			p, _ := t.a.procs.get(id)
			fmt.Fprintf(&b, "%s (pid %d): %s — %s\n", id, p.PID(), p.Status(), p.Command)
		}
		return outcome{result: strings.TrimRight(b.String(), "\n")}
	}
	p, ok := t.a.procs.get(id)
	if !ok {
		return outcome{err: fmt.Errorf("read_process_output: no background process %q (call it without id to list them)", id)}
	}
	var re *regexp.Regexp
	if pat, _ := call.Args["pattern"].(string); pat != "" {
		var err error
		if re, err = regexp.Compile(pat); err != nil {
			return outcome{err: fmt.Errorf("read_process_output: bad pattern: %w", err)}
		}
	}
	var wait time.Duration
	if w, ok := call.Args["wait_seconds"].(float64); ok && w > 0 {
		wait = time.Duration(min(w, maxProcWait) * float64(time.Second))
	}
	// Wait for new output; with a pattern, for a matching line.
	deadline := time.Now().Add(wait)
	out := ""
	for {
		out += p.NewOutput(procOutputMax)
		if (strings.TrimSpace(out) != "" && (re == nil || re.MatchString(out))) || !p.Running() || ctx.Err() != nil {
			break
		}
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		p.WaitForOutput(ctx, left)
	}
	if len(out) > procOutputMax {
		out = "[showing the most recent output]\n" + out[len(out)-procOutputMax:]
	}
	if re != nil {
		var keep []string
		for _, line := range strings.Split(out, "\n") {
			if re.MatchString(line) {
				keep = append(keep, line)
			}
		}
		out = strings.Join(keep, "\n")
		if out == "" {
			out = "(no new lines match the pattern)"
		}
	} else if strings.TrimSpace(out) == "" {
		out = "(no new output)"
	}
	return outcome{result: fmt.Sprintf("%s: %s\n%s", id, p.Status(), out)}
}

// killProcess runs kill_process.
func (t *turn) killProcess(call llm.ToolCall) outcome {
	id, _ := call.Args["id"].(string)
	p, ok := t.a.procs.get(strings.TrimSpace(id))
	if !ok {
		return outcome{err: fmt.Errorf("kill_process: no background process %q", id)}
	}
	t.emit(ToolStartEvent{Call: call, Auto: true})
	if !p.Running() {
		return outcome{result: fmt.Sprintf("%s had already %s.\n--- last output ---\n%s", id, p.Status(), p.Tail(4096))}
	}
	p.Kill()
	return outcome{result: fmt.Sprintf("Stopped %s: %s.\n--- last output ---\n%s", id, p.Status(), p.Tail(4096))}
}
