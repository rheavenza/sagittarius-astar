package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOpenAIServer serves /models and a streaming /chat/completions whose
// chunks are given by sse. It records the last chat request body.
func fakeOpenAIServer(t *testing.T, sse []string, lastBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			fmt.Fprint(w, `{"object":"list","data":[{"id":"z/model","object":"model"},{"id":"a/model","object":"model"}]}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			b, _ := io.ReadAll(r.Body)
			if lastBody != nil {
				_ = json.Unmarshal(b, lastBody)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			for _, chunk := range sse {
				fmt.Fprintf(w, "data: %s\n\n", chunk)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

func drain(ch chan string) string {
	var sb strings.Builder
	for tok := range ch {
		sb.WriteString(tok)
	}
	return sb.String()
}

func TestNIMToolCallsWithoutToolCallsFinishReason(t *testing.T) {
	// vLLM-style: tool call deltas, then finish_reason "stop" and no ID.
	sse := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"go.mod\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	var body map[string]any
	srv := fakeOpenAIServer(t, sse, &body)
	defer srv.Close()

	p, err := NewNIMProvider("nvapi-test", "meta/llama-3.3-70b-instruct", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ch := make(chan string, 16)
	history := []Message{
		{Role: RoleSystem, Content: "persona"},
		{Role: RoleUser, Content: "read go.mod"},
		{Role: RoleAssistant, Content: "earlier", ReasoningContent: "secret thoughts"},
		{Role: RoleSystem, Content: "Command executed: ls"},
		{Role: RoleUser, Content: "next"},
	}
	calls, _, err := p.StreamChat(context.Background(), history,
		[]Tool{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}, ch)
	drain(ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "go.mod" {
		t.Fatalf("tool call not recovered: %+v", calls)
	}
	if calls[0].ID == "" {
		t.Error("missing tool call ID should be synthesized")
	}

	// Request shape: no reasoning_content echoed, late system folded into user.
	msgs := body["messages"].([]any)
	for _, raw := range msgs {
		m := raw.(map[string]any)
		if _, ok := m["reasoning_content"]; ok {
			t.Error("reasoning_content must not be sent to NIM")
		}
	}
	systemCount := 0
	for _, raw := range msgs {
		if raw.(map[string]any)["role"] == "system" {
			systemCount++
		}
	}
	if systemCount != 1 {
		t.Errorf("expected exactly one system message, got %d", systemCount)
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" || !strings.Contains(last["content"].(string), "[system note]") || !strings.Contains(last["content"].(string), "next") {
		t.Errorf("late system message should be folded into the following user turn: %v", last)
	}
	if body["tool_choice"] != "auto" {
		t.Errorf("tool_choice should be auto when tools are sent, got %v", body["tool_choice"])
	}
}

func TestNIMTextStreamAndNoToolChoiceWithoutTools(t *testing.T) {
	sse := []string{
		`{"choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
	}
	var body map[string]any
	srv := fakeOpenAIServer(t, sse, &body)
	defer srv.Close()

	p, _ := NewNIMProvider("", "m", srv.URL)
	ch := make(chan string, 16)
	calls, _, err := p.StreamChat(context.Background(), []Message{{Role: RoleUser, Content: "hi"}}, nil, ch)
	if err != nil {
		t.Fatal(err)
	}
	if got := drain(ch); got != "Hello" || calls != nil {
		t.Fatalf("got text %q calls %v", got, calls)
	}
	if _, ok := body["tool_choice"]; ok {
		t.Error("tool_choice must be omitted when no tools are sent")
	}
}

func TestDeepSeekStillEchoesReasoning(t *testing.T) {
	var body map[string]any
	srv := fakeOpenAIServer(t, []string{`{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`}, &body)
	defer srv.Close()

	p := newOpenAICompat("deepseek", "k", "deepseek-v4-pro", srv.URL, true)
	ch := make(chan string, 4)
	_, _, err := p.StreamChat(context.Background(), []Message{
		{Role: RoleUser, Content: "q"},
		{Role: RoleAssistant, Content: "a", ReasoningContent: "r"},
		{Role: RoleUser, Content: "q2"},
	}, nil, ch)
	drain(ch)
	if err != nil {
		t.Fatal(err)
	}
	assistant := body["messages"].([]any)[1].(map[string]any)
	if assistant["reasoning_content"] != "r" {
		t.Errorf("DeepSeek must echo reasoning_content, got %v", assistant)
	}
}

func TestListOpenAICompatModels(t *testing.T) {
	srv := fakeOpenAIServer(t, nil, nil)
	defer srv.Close()
	ids, err := ListOpenAICompatModels(context.Background(), "k", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a/model" {
		t.Fatalf("expected sorted IDs, got %v", ids)
	}
}

func TestNIMRequiresModel(t *testing.T) {
	if _, err := NewNIMProvider("k", "", ""); err == nil {
		t.Fatal("expected an error for an empty model")
	}
}
