package llm

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestBuildGeminiContentsKeepsFirstSystemPrompt(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "PERSONA"},
		{Role: RoleUser, Content: "run ls"},
		{Role: RoleAssistant, Content: "ok"},
		{Role: RoleSystem, Content: "Command executed: ls"},
		{Role: RoleUser, Content: "thanks"},
	}
	contents, sys := buildGeminiContents(msgs)

	if sys == nil || sys.Parts[0].Text != "PERSONA" {
		t.Fatalf("system instruction must be the first system message, got %+v", sys)
	}
	// The later system message becomes a user-role note merged with "thanks".
	last := contents[len(contents)-1]
	if last.Role != genai.RoleUser || len(last.Parts) != 2 {
		t.Fatalf("expected merged user content with note + text, got role=%s parts=%d", last.Role, len(last.Parts))
	}
	if !strings.HasPrefix(last.Parts[0].Text, "[system note]") {
		t.Errorf("expected system note, got %q", last.Parts[0].Text)
	}
}

func TestBuildGeminiContentsToolRoundTrip(t *testing.T) {
	sig := []byte("sig")
	msgs := []Message{
		{Role: RoleUser, Content: "read two files"},
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "1", Name: "read_file", Args: map[string]any{"path": "a"}, ThoughtSignature: sig},
			{ID: "2", Name: "read_file", Args: map[string]any{"path": "b"}},
		}},
		{Role: RoleTool, Content: "A", ToolCallID: "1", ToolName: "read_file"},
		{Role: RoleTool, Content: "B", ToolCallID: "2", ToolName: "read_file"},
	}
	contents, _ := buildGeminiContents(msgs)
	if len(contents) != 3 {
		t.Fatalf("expected user, model, merged-responses; got %d contents", len(contents))
	}
	model := contents[1]
	if string(model.Parts[0].ThoughtSignature) != "sig" {
		t.Error("thought signature not echoed on function call part")
	}
	responses := contents[2]
	if len(responses.Parts) != 2 || responses.Parts[0].FunctionResponse == nil {
		t.Fatalf("parallel function responses should be merged into one content: %+v", responses)
	}
	if responses.Parts[1].FunctionResponse.Response["output"] != "B" {
		t.Errorf("non-JSON tool output should be wrapped as {output: ...}")
	}
}

func TestGeminiSupportsThinking(t *testing.T) {
	cases := map[string]bool{
		"gemini-2.0-flash":     false,
		"gemini-1.5-pro":       false,
		"gemini-2.5-flash":     true,
		"gemini-2.5-pro":       true,
		"gemini-3-pro-preview": true,
	}
	for model, want := range cases {
		if got := geminiSupportsThinking(model); got != want {
			t.Errorf("%s: got %v want %v", model, got, want)
		}
	}
}
