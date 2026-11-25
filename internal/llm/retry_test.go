package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
)

func TestIsRetryable(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("deepseek: failed to create stream: %w", err) }
	cases := []struct {
		err  error
		want bool
	}{
		{wrap(&openai.APIError{HTTPStatusCode: 429, Message: "rate limited"}), true},
		{wrap(&openai.APIError{HTTPStatusCode: 503}), true},
		{wrap(&openai.RequestError{HTTPStatusCode: 502}), true},
		{wrap(&openai.APIError{HTTPStatusCode: 400, Message: "bad request"}), false},
		{wrap(&openai.APIError{HTTPStatusCode: 401}), false},
		{fmt.Errorf("gemini: stream error: %w", genai.APIError{Code: 503}), true},
		{fmt.Errorf("gemini: stream error: %w", genai.APIError{Code: 429}), true},
		{fmt.Errorf("gemini: stream error: %w", genai.APIError{Code: 404}), false},
		{wrap(io.ErrUnexpectedEOF), true},
		{context.Canceled, false},
		{wrap(context.DeadlineExceeded), false},
		{errors.New("something else"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
	if got := HTTPStatus(wrap(&openai.APIError{HTTPStatusCode: 429})); got != 429 {
		t.Errorf("HTTPStatus = %d, want 429", got)
	}
}
