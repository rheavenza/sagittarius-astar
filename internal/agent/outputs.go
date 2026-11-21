package agent

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
)

// Large tool outputs are kept out of the conversation: the history gets the
// head and tail plus an id, and read_output fetches any part on demand. This
// keeps one big command output or file from filling the context for the
// rest of the session.

const (
	// largeOutputBytes: results above this are stored and shortened.
	largeOutputBytes = 16 * 1024
	// outputPreviewBytes of head and of tail stay in the history.
	outputPreviewBytes = 3 * 1024
	// maxStoredBytes bounds the store; the oldest outputs are evicted.
	maxStoredBytes = 32 << 20
	// readOutputMax bounds one read_output answer.
	readOutputMax = 16 * 1024
)

type outputStore struct {
	mu    sync.Mutex
	seq   int
	order []string
	data  map[string]string
	size  int
}

// put stores content and returns its id.
func (s *outputStore) put(content string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]string{}
	}
	s.seq++
	id := fmt.Sprintf("out-%d", s.seq)
	s.data[id] = content
	s.order = append(s.order, id)
	s.size += len(content)
	for s.size > maxStoredBytes && len(s.order) > 1 {
		old := s.order[0]
		s.order = s.order[1:]
		s.size -= len(s.data[old])
		delete(s.data, old)
	}
	return id
}

func (s *outputStore) get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *outputStore) empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq == 0
}

// shrinkOutput stores a large result and returns the preview that goes into
// the history; small results are returned unchanged.
func (a *Agent) shrinkOutput(toolName, content string) string {
	if len(content) <= largeOutputBytes || toolName == "read_output" || toolName == "explore" {
		return content
	}
	id := a.outputs.put(content)
	lines := strings.Count(content, "\n") + 1
	head := tools.TruncateUTF8(content, outputPreviewBytes)
	tail := content[len(content)-outputPreviewBytes:]
	for len(tail) > 0 && tail[0]&0xC0 == 0x80 {
		tail = tail[1:]
	}
	return fmt.Sprintf("%s\n\n…[output truncated: %d bytes, %d lines. The full output is stored as %q: call read_output with that id and start_line/end_line, or a pattern to search it.]…\n\n%s",
		head, len(content), lines, id, tail)
}

// readOutputDefinition describes read_output; it is offered once something
// was stored.
func readOutputDefinition() llm.Tool {
	return llm.Tool{
		Name:        "read_output",
		Description: "Read part of a large tool output that was truncated in the conversation (its id, like \"out-3\", is given in the truncation note). Use start_line/end_line for a range, or pattern (a regular expression) to get the matching lines with their line numbers.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":         map[string]any{"type": "string", "description": "Output id from the truncation note"},
				"start_line": map[string]any{"type": "integer", "description": "First line (1-based, default 1)"},
				"end_line":   map[string]any{"type": "integer", "description": "Last line (inclusive, default start_line+199)"},
				"pattern":    map[string]any{"type": "string", "description": "Regular expression; returns matching lines (max 200) instead of a range"},
			},
			"required": []string{"id"},
		},
	}
}

func (a *Agent) readOutput(call llm.ToolCall) outcome {
	id, _ := call.Args["id"].(string)
	content, ok := a.outputs.get(strings.TrimSpace(id))
	if !ok {
		return outcome{err: fmt.Errorf("read_output: no stored output %q (outputs are kept for this session only, and the oldest are evicted)", id)}
	}
	lines := strings.Split(content, "\n")

	if pat, _ := call.Args["pattern"].(string); pat != "" {
		re, err := regexp.Compile(pat)
		if err != nil {
			return outcome{err: fmt.Errorf("read_output: invalid pattern: %w", err)}
		}
		var b strings.Builder
		n := 0
		for i, l := range lines {
			if !re.MatchString(l) {
				continue
			}
			if n == 200 || b.Len() > readOutputMax {
				b.WriteString("…(more matches; narrow the pattern)\n")
				break
			}
			fmt.Fprintf(&b, "%6d\t%s\n", i+1, tools.TruncateUTF8(l, 500))
			n++
		}
		if n == 0 {
			return outcome{result: fmt.Sprintf("No lines of %s match %q (%d lines total).", id, pat, len(lines))}
		}
		return outcome{result: fmt.Sprintf("%d matching lines of %s:\n%s", n, id, b.String())}
	}

	start := 1
	if v, ok := call.Args["start_line"].(float64); ok && v >= 1 {
		start = int(v)
	}
	end := start + 199
	if v, ok := call.Args["end_line"].(float64); ok && int(v) >= start {
		end = int(v)
	}
	if start > len(lines) {
		return outcome{err: fmt.Errorf("read_output: %s has %d lines", id, len(lines))}
	}
	end = min(end, len(lines))
	var b strings.Builder
	fmt.Fprintf(&b, "%s lines %d-%d of %d:\n", id, start, end, len(lines))
	for i := start; i <= end; i++ {
		line := fmt.Sprintf("%6d\t%s\n", i, lines[i-1])
		if b.Len()+len(line) > readOutputMax {
			fmt.Fprintf(&b, "…(stopped at line %d: size limit; continue with start_line=%d)\n", i-1, i)
			break
		}
		b.WriteString(line)
	}
	return outcome{result: b.String()}
}
