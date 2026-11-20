package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
)

// Tool is the MCP wire representation of a tool as returned by a tools/list RPC.
// We preserve the raw JSON Schema in InputSchema for protocol fidelity before
// translating into provider-specific types in LLMTools().
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"` // raw JSON Schema object
}

// toolsListResult is the unmarshalling target for a successful tools/list response.
type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

// toolCallParams is the JSON-RPC params object for a tools/call request.
type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// toolCallResult is the unmarshalling target for a successful tools/call response.
type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// toolContent is a single content block from a tool response.
// MCP defines several content types; we inline "text" and summarise the rest.
type toolContent struct {
	Type     string `json:"type"` // "text" | "image" | "audio" | "resource" | "resource_link"
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
}

// ToolNamePrefix is prepended to every MCP tool exposed to the LLM:
// mcp__<server>__<tool>. Namespacing prevents MCP tools from shadowing native
// tools (e.g. the filesystem server's write_file) and from colliding with each
// other across servers.
const ToolNamePrefix = "mcp__"

// maxToolNameLen is the strictest provider limit (Gemini/OpenAI: 64 chars).
const maxToolNameLen = 64

// toolRef locates a namespaced tool: which server owns it and its original name.
type toolRef struct {
	server string
	name   string
}

// Registry discovers and indexes tools across all connected MCP servers.
type Registry struct {
	clients map[string]*Client // logical server name → JSON-RPC client
	trusted map[string]bool    // server name → skip confirmation
	toolMap map[string]toolRef // exposed (namespaced) name → owner
	tools   []Tool             // discovered tools; Name is the exposed name

	mu sync.RWMutex
}

// NewRegistry builds a Registry by creating a Client for each live server in m.
// Call Discover() after NewRegistry() to populate the tool index.
func NewRegistry(m *Manager) *Registry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	clients := make(map[string]*Client, len(m.servers))
	trusted := make(map[string]bool, len(m.servers))
	for name, srv := range m.servers {
		clients[name] = NewClient(srv)
		trusted[name] = srv.Config.Trusted
	}

	return &Registry{
		clients: clients,
		trusted: trusted,
		toolMap: make(map[string]toolRef),
	}
}

// sanitizeName maps s onto the character set accepted by every provider.
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// ExposedName returns the namespaced tool name sent to the LLM.
func ExposedName(server, tool string) string {
	name := ToolNamePrefix + sanitizeName(server) + "__" + sanitizeName(tool)
	if len(name) > maxToolNameLen {
		name = name[:maxToolNameLen]
	}
	return name
}

// Discover initializes every server and lists its tools, concurrently.
// A server that fails is logged, closed, and dropped; the rest keep working.
// It is safe to call multiple times; subsequent calls refresh the index.
func (r *Registry) Discover(ctx context.Context) error {
	type discovered struct {
		server string
		tools  []Tool
		err    error
	}

	r.mu.RLock()
	clients := make(map[string]*Client, len(r.clients))
	for k, v := range r.clients {
		clients[k] = v
	}
	r.mu.RUnlock()

	results := make(chan discovered, len(clients))
	var wg sync.WaitGroup
	for name, c := range clients {
		wg.Add(1)
		go func(name string, c *Client) {
			defer wg.Done()
			tools, err := discoverServer(ctx, c)
			results <- discovered{server: name, tools: tools, err: err}
		}(name, c)
	}
	wg.Wait()
	close(results)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.tools = r.tools[:0]
	r.toolMap = make(map[string]toolRef)

	var failed []string
	var all []discovered
	for d := range results {
		all = append(all, d)
	}
	// Deterministic order for the tool list sent to the LLM.
	sort.Slice(all, func(i, j int) bool { return all[i].server < all[j].server })

	for _, d := range all {
		if d.err != nil {
			logger.L.Warn("mcp: server discovery failed, skipping it", "server", d.server, "error", d.err)
			failed = append(failed, d.server)
			if c := r.clients[d.server]; c != nil {
				go c.Close()
			}
			delete(r.clients, d.server)
			continue
		}
		for _, t := range d.tools {
			exposed := ExposedName(d.server, t.Name)
			if _, dup := r.toolMap[exposed]; dup {
				logger.L.Warn("mcp: duplicate tool name after namespacing, skipping", "server", d.server, "tool", t.Name)
				continue
			}
			r.toolMap[exposed] = toolRef{server: d.server, name: t.Name}
			t.Name = exposed
			r.tools = append(r.tools, t)
		}
	}

	if len(r.clients) == 0 && len(failed) > 0 {
		return fmt.Errorf("mcp: all servers failed discovery: %s", strings.Join(failed, ", "))
	}
	return nil
}

// discoverServer runs initialize + tools/list against one server.
func discoverServer(ctx context.Context, c *Client) ([]Tool, error) {
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	raw, err := c.Call(ctx, "tools/list", nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	var result toolsListResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse tools/list response: %w", err)
	}
	return result.Tools, nil
}

// LLMTools translates all discovered MCP tools into the []llm.Tool slice
// that is injected into Provider.StreamChat() alongside the native agent tools.
// Names are the namespaced mcp__<server>__<tool> form.
//
// If schema unmarshalling fails for a tool, a bare {"type":"object"} is
// substituted so the tool remains callable with untyped arguments.
func (r *Registry) LLMTools() []llm.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]llm.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out = append(out, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  schema,
		})
	}
	return out
}

// Call invokes a namespaced MCP tool on its server and returns the text of the
// response. Non-text blocks are summarised as placeholders.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	r.mu.RLock()
	ref, ok := r.toolMap[name]
	client := r.clients[ref.server]
	r.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("mcp: tool %q is not registered", name)
	}
	if client == nil {
		return "", fmt.Errorf("mcp: tool %q: server %q has no active client", name, ref.server)
	}

	params := toolCallParams{
		Name:      ref.name,
		Arguments: args,
	}
	if params.Arguments == nil {
		params.Arguments = make(map[string]any)
	}

	raw, err := client.Call(ctx, "tools/call", params)
	if err != nil {
		return "", fmt.Errorf("mcp: tool %q: rpc call failed: %w", name, err)
	}

	var result toolCallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("mcp: tool %q: parse response: %w", name, err)
	}

	var parts []string
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		case "resource", "resource_link":
			parts = append(parts, fmt.Sprintf("[%s omitted: %s]", c.Type, c.URI))
		default:
			parts = append(parts, fmt.Sprintf("[%s content omitted (%s)]", c.Type, c.MimeType))
		}
	}
	text := strings.Join(parts, "\n")

	if result.IsError {
		if text == "" {
			text = "tool reported an error with no detail"
		}
		return "", fmt.Errorf("mcp: tool %q error: %s", name, text)
	}

	return text, nil
}

// IsMCPTool returns true if name is a namespaced tool from any MCP server.
func (r *Registry) IsMCPTool(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.toolMap[name]
	return ok
}

// IsTrusted reports whether the server owning the named tool is marked
// "trusted" in config (its tools run without a confirmation prompt).
func (r *Registry) IsTrusted(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.toolMap[name]
	return ok && r.trusted[ref.server]
}

// ToolCount returns the total number of MCP tools discovered across all servers.
func (r *Registry) ToolCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// Shutdown closes all JSON-RPC client read loops. Call this before Manager.Shutdown()
// to ensure all in-flight RPC calls are drained before the pipes are closed.
func (r *Registry) Shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.clients {
		c.Close()
	}
}
