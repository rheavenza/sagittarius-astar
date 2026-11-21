package llm

import (
	"context"
	"strings"
)

// Usage is the token accounting of one model request, as reported by the
// API. Zero fields mean "not reported".
type Usage struct {
	InputTokens  int // prompt tokens, including cached ones
	OutputTokens int // completion tokens (including reasoning)
	CachedTokens int // prompt tokens served from the provider's cache
}

type usageKey struct{}

// WithUsage returns a context that collects the usage of a StreamChat call
// made with it into u. Providers that can't report usage leave u untouched.
// (A context value instead of a return value keeps the Provider interface,
// and every test fake implementing it, unchanged.)
func WithUsage(ctx context.Context, u *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, u)
}

// ReportUsage stores u in the context's collector, if any. Providers call
// it when the API reports usage (exported for test fakes).
func ReportUsage(ctx context.Context, u Usage) {
	if dst, ok := ctx.Value(usageKey{}).(*Usage); ok && dst != nil && (u.InputTokens > 0 || u.OutputTokens > 0) {
		*dst = u
	}
}

// DefaultContextWindow is used for models missing from the table.
const DefaultContextWindow = 32_768

// contextWindows maps model-name prefixes (lower-case) to context windows in
// tokens. The longest matching prefix wins. Override per model with
// "context_windows" in config.json.
var contextWindows = map[string]int{
	"gemini-1.5":        1_048_576,
	"gemini-2":          1_048_576,
	"gemini-3":          1_048_576,
	"deepseek":          131_072,
	"meta/llama-3.1":    131_072,
	"meta/llama-3.3":    131_072,
	"meta/llama-4":      131_072,
	"moonshotai/kimi":   131_072,
	"google/gemma-3":    131_072,
	"google/gemma-4":    131_072,
	"qwen/":             131_072,
	"mistralai/":        131_072,
	"nvidia/":           131_072,
	"openai/gpt-oss":    131_072,
	"deepseek-ai/":      131_072,
	"microsoft/phi-4":   131_072,
	"ibm/granite":       131_072,
	"meta/llama3-70b":   8_192,
	"meta/llama3-8b":    8_192,
	"google/gemma-2":    8_192,
	"mistralai/mistral": 32_768,
}

// ContextWindow returns the context window of model in tokens. overrides
// (from config, keyed by exact model name) win over the built-in table.
func ContextWindow(model string, overrides map[string]int) int {
	if n := overrides[model]; n > 0 {
		return n
	}
	m := strings.ToLower(model)
	best, n := 0, DefaultContextWindow
	for prefix, w := range contextWindows {
		if strings.HasPrefix(m, prefix) && len(prefix) > best {
			best, n = len(prefix), w
		}
	}
	return n
}
