package gateway

import (
	"fmt"
	"time"

	"github.com/abdullah-9211/spillway/internal/usage"
)

// Error is a failure the API reports in the OpenAI error shape.
type Error struct {
	Status  int
	Type    string
	Code    string
	Param   string
	Message string
	// Attempts is set on failures that tried providers, and sent to the client as `attempts`.
	Attempts []usage.Attempt
	// RetryAfter is sent as the Retry-After header on 429s.
	RetryAfter time.Duration
}

func (e *Error) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func invalid(param, msg string, args ...any) *Error {
	return &Error{Status: 400, Type: "invalid_request_error", Code: "invalid_request", Param: param, Message: fmt.Sprintf(msg, args...)}
}

func upstream(msg string, args ...any) *Error {
	return &Error{Status: 502, Type: "api_error", Code: "upstream_error", Message: fmt.Sprintf(msg, args...)}
}
