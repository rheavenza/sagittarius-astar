package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/repomap"
)

const (
	defaultRepoMapTokens = 4096
	maxRepoMapTokens     = 16384
	repoMapTimeout       = 30 * time.Second
)

func repoMap(call llm.ToolCall) (string, *PendingWrite, error) {
	dir, _ := call.Args["dir"].(string)
	if dir == "" {
		dir = "."
	}
	budget := defaultRepoMapTokens
	if v, ok := call.Args["max_tokens"].(float64); ok && v > 0 {
		budget = min(int(v), maxRepoMapTokens)
	}
	focus := stringList(call.Args["focus"])

	ctx, cancel := context.WithTimeout(context.Background(), repoMapTimeout)
	defer cancel()
	m, err := repomap.Build(ctx, dir)
	if err != nil {
		return "", nil, fmt.Errorf("repo_map: %w", err)
	}
	out := m.Render(repomap.Options{MaxTokens: budget, Focus: focus, LineNumbers: true})
	files, syms := m.Stats()
	if out == "" {
		return fmt.Sprintf("repo_map: no definitions found in %s (%d source files; supported: Go, Python, JS/TS, Rust, shell)", dir, files), nil, nil
	}
	header := fmt.Sprintf("Repository map of %s: %d source files, %d definitions; ranked by references, line numbers on the left.", dir, files, syms)
	if len(focus) > 0 {
		header += " Focus: " + strings.Join(focus, ", ") + "."
	}
	return header + "\n" + out, nil, nil
}
