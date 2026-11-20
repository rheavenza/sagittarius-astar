// Package llm — OpenAI-compatible provider implementation.
//
// DeepSeek and NVIDIA NIM (hosted at integrate.api.nvidia.com or self-hosted)
// both speak the OpenAI Chat Completions API, so one implementation built on
// github.com/sashabaranov/go-openai serves both; only the base URL and a few
// quirks differ.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	openai "github.com/sashabaranov/go-openai"

	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
)

// Default base URLs for OpenAI-compatible backends.
const (
	DeepSeekBaseURL = "https://api.deepseek.com"
	NIMBaseURL      = "https://integrate.api.nvidia.com/v1"
)

// OpenAICompatProvider implements Provider for any OpenAI-compatible API.
type OpenAICompatProvider struct {
	client *openai.Client
	model  string
	name   string // for logs and errors: "deepseek", "nim"

	// echoReasoning sends each assistant message's ReasoningContent back as
	// reasoning_content. DeepSeek thinking models require this during tool
	// loops; other backends don't define the field, so it is off for them.
	echoReasoning bool

	// foldLateSystem turns every system message after the first into a
	// "[system note]" user message (merged with an adjacent user message).
	// Many open-model chat templates (Gemma, Mistral, …) reject system
	// messages mid-conversation or require strict user/assistant alternation.
	foldLateSystem bool
}

// DeepSeekProvider is kept as a name for the DeepSeek flavour.
type DeepSeekProvider = OpenAICompatProvider

// NewDeepSeekProvider creates an authenticated DeepSeek provider.
// apiKey must be a valid DeepSeek API key.
func NewDeepSeekProvider(apiKey, model string) (*OpenAICompatProvider, error) {
	return newOpenAICompat("deepseek", apiKey, model, DeepSeekBaseURL, true), nil
}

// NewNIMProvider creates an NVIDIA NIM provider. baseURL may be empty for the
// hosted API (integrate.api.nvidia.com) or point at a self-hosted NIM, e.g.
// http://localhost:8000/v1. apiKey may be empty for a self-hosted NIM.
func NewNIMProvider(apiKey, model, baseURL string) (*OpenAICompatProvider, error) {
	if baseURL == "" {
		baseURL = NIMBaseURL
	}
	if model == "" {
		return nil, fmt.Errorf("nim: a model name is required (e.g. meta/llama-3.3-70b-instruct)")
	}
	p := newOpenAICompat("nim", apiKey, model, strings.TrimRight(baseURL, "/"), false)
	p.foldLateSystem = true
	return p, nil
}

func newOpenAICompat(name, apiKey, model, baseURL string, echoReasoning bool) *OpenAICompatProvider {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL
	return &OpenAICompatProvider{
		client:        openai.NewClientWithConfig(cfg),
		model:         model,
		name:          name,
		echoReasoning: echoReasoning,
	}
}

// ListOpenAICompatModels returns the model IDs served by an OpenAI-compatible
// endpoint (GET /models), sorted alphabetically.
func ListOpenAICompatModels(ctx context.Context, apiKey, baseURL string) ([]string, error) {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = strings.TrimRight(baseURL, "/")
	list, err := openai.NewClientWithConfig(cfg).ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models from %s: %w", cfg.BaseURL, err)
	}
	ids := make([]string, 0, len(list.Models))
	for _, m := range list.Models {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids, nil
}

// StreamChat streams a response from the backend into tokenChan.
// The channel is always closed before this function returns.
//
// Returns (toolCalls, reasoningContent, error):
//   - toolCalls: non-nil when the model requested tool execution.
//   - reasoningContent: chain-of-thought text emitted by DeepSeek thinking
//     models (deepseek-v4-pro / deepseek-reasoner). Callers MUST store this in
//     the assistant Message and echo it on the next request or the API returns
//     HTTP 400 ("reasoning_content must be passed back").
//   - err: any transport or API error.
func (d *OpenAICompatProvider) StreamChat(ctx context.Context, messages []Message, tools []Tool, tokenChan chan<- string) ([]ToolCall, string, error) {
	defer close(tokenChan)

	logger.L.Debug(d.name+": stream starting",
		"model", d.model,
		"history_len", len(messages),
	)
	tokenCount := 0
	defer func() {
		logger.L.Debug(d.name+": stream finished", "tokens_received", tokenCount)
	}()

	if d.foldLateSystem {
		messages = foldLateSystemMessages(messages)
	}

	// Map our internal Message type to openai.ChatCompletionMessage.
	oaiMessages := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, msg := range messages {
		var role string
		switch msg.Role {
		case RoleSystem:
			role = openai.ChatMessageRoleSystem
		case RoleAssistant:
			role = openai.ChatMessageRoleAssistant
		case RoleTool:
			role = openai.ChatMessageRoleTool
		default:
			role = openai.ChatMessageRoleUser
		}

		oaiMsg := openai.ChatCompletionMessage{
			Role:       role,
			Content:    msg.Content,
			Name:       msg.ToolName,
			ToolCallID: msg.ToolCallID,
		}
		if d.echoReasoning {
			// DeepSeek thinking models require reasoning_content to be echoed back.
			oaiMsg.ReasoningContent = msg.ReasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				b, _ := json.Marshal(tc.Args)
				oaiMsg.ToolCalls = append(oaiMsg.ToolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(b),
					},
				})
			}
		}
		oaiMessages = append(oaiMessages, oaiMsg)
	}

	req := openai.ChatCompletionRequest{
		Model:    d.model,
		Messages: oaiMessages,
		Stream:   true,
	}

	// tool_choice is only valid when tools are supplied; OpenAI-compatible
	// APIs reject it otherwise.
	if len(tools) > 0 {
		req.ToolChoice = "auto"
		for _, t := range tools {
			req.Tools = append(req.Tools, openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
	}

	stream, err := d.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		return nil, "", fmt.Errorf("%s: failed to create stream: %w", d.name, err)
	}
	defer stream.Close()

	// pendingCalls accumulates tool calls across streaming deltas.
	// DeepSeek sends them index-by-index with incremental argument JSON.
	pendingCalls := make(map[int]*struct {
		id   string
		name string
		args string
	})

	// collectCalls turns the accumulated deltas into ToolCalls, in index order.
	collectCalls := func() []ToolCall {
		idxs := make([]int, 0, len(pendingCalls))
		for i := range pendingCalls {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		var calls []ToolCall
		for n, i := range idxs {
			entry := pendingCalls[i]
			var args map[string]any
			if strings.TrimSpace(entry.args) == "" {
				args = map[string]any{}
			} else if err := json.Unmarshal([]byte(entry.args), &args); err != nil {
				args = map[string]any{"raw": entry.args}
			}
			id := entry.id
			if id == "" {
				// Some servers omit IDs; tool results must still reference one.
				id = fmt.Sprintf("call_%d", n)
			}
			calls = append(calls, ToolCall{ID: id, Name: entry.name, Args: args})
		}
		return calls
	}

	// reasoningBuf accumulates chain-of-thought tokens from thinking models.
	// These arrive in Delta.ReasoningContent before regular content tokens.
	var reasoningBuf strings.Builder

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			logger.L.Error(d.name+": stream recv error", "error", err)
			return nil, "", fmt.Errorf("%s: stream error: %w", d.name, err)
		}

		if len(resp.Choices) == 0 {
			continue
		}

		choice := resp.Choices[0]

		// Collect chain-of-thought tokens (thinking models only).
		if rc := choice.Delta.ReasoningContent; rc != "" {
			reasoningBuf.WriteString(rc)
			select {
			case <-ctx.Done():
				logger.L.Warn(d.name+": stream cancelled by context", "tokens_received", tokenCount)
				return nil, "", ctx.Err()
			case tokenChan <- "\x00" + rc:
			}
		}

		// Accumulate all tool call deltas by index.
		for _, tc := range choice.Delta.ToolCalls {
			idx := tc.Index
			if idx == nil {
				i := 0
				idx = &i
			}
			entry := pendingCalls[*idx]
			if entry == nil {
				entry = &struct {
					id   string
					name string
					args string
				}{}
				pendingCalls[*idx] = entry
			}
			if tc.ID != "" {
				entry.id = tc.ID
			}
			if tc.Function.Name != "" {
				entry.name = tc.Function.Name
			}
			entry.args += tc.Function.Arguments
		}

		if choice.FinishReason == openai.FinishReasonToolCalls {
			calls := collectCalls()
			logger.L.Debug(d.name+": tool calls received", "count", len(calls))
			// Return reasoning content alongside the tool calls so the caller
			// can atomically store it in the assistant message. This avoids any
			// race between the token channel and the toolCallBatchMsg.
			return calls, reasoningBuf.String(), nil
		}

		delta := choice.Delta.Content
		if delta == "" {
			continue
		}

		tokenCount++
		select {
		case <-ctx.Done():
			logger.L.Warn(d.name+": stream cancelled by context", "tokens_received", tokenCount)
			return nil, "", ctx.Err()
		case tokenChan <- delta:
		}
	}

	// Some OpenAI-compatible servers (e.g. vLLM-based NIMs) finish a tool
	// turn with finish_reason "stop" or none at all; honour any calls that
	// were streamed anyway.
	if len(pendingCalls) > 0 {
		calls := collectCalls()
		logger.L.Debug(d.name+": tool calls received at end of stream", "count", len(calls))
		return calls, reasoningBuf.String(), nil
	}

	// Non-tool turn: return any reasoning content accumulated (e.g. when a
	// thinking model responds without calling tools).
	return nil, reasoningBuf.String(), nil
}

// foldLateSystemMessages keeps the first system message as-is and rewrites
// later ones as user-role "[system note]" text, merging consecutive user
// messages so roles keep alternating.
func foldLateSystemMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	seenSystem := false
	for _, msg := range messages {
		if msg.Role == RoleSystem {
			if !seenSystem && len(out) == 0 {
				seenSystem = true
				out = append(out, msg)
				continue
			}
			msg = Message{Role: RoleUser, Content: "[system note]\n" + msg.Content}
		}
		if msg.Role == RoleUser && len(out) > 0 && out[len(out)-1].Role == RoleUser {
			out[len(out)-1].Content += "\n\n" + msg.Content
			continue
		}
		out = append(out, msg)
	}
	return out
}
