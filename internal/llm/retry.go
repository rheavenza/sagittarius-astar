package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	openai "github.com/sashabaranov/go-openai"
	"google.golang.org/genai"
)

// retryableStatus are HTTP statuses worth retrying: rate limits and
// temporary server trouble (529 is "overloaded" on some providers).
var retryableStatus = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true, 529: true}

// IsRetryable reports whether err from StreamChat is likely transient: a
// rate limit, a 5xx from the provider, or a dropped connection. Cancellation
// and deadlines are not retryable, and neither are client errors (bad key,
// bad request, unknown model).
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if code := HTTPStatus(err); code != 0 {
		return retryableStatus[code]
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// HTTPStatus extracts the HTTP status code from a provider error, or 0.
func HTTPStatus(err error) int {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatusCode != 0 {
		return apiErr.HTTPStatusCode
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) && reqErr.HTTPStatusCode != 0 {
		return reqErr.HTTPStatusCode
	}
	var gv genai.APIError
	if errors.As(err, &gv) {
		return gv.Code
	}
	var gp *genai.APIError
	if errors.As(err, &gp) && gp != nil {
		return gp.Code
	}
	return 0
}
