package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

type ErrKind int

const (
	KindRateLimited   ErrKind = iota // 429
	KindServer                       // 5xx or a broken connection
	KindTimeout                      // deadline exceeded
	KindBadRequest                   // 4xx the client caused: never retried, never failed over
	KindAuth                         // bad upstream credentials: fails over, logged loudly
	KindContextLength                // fails over only to a model with a larger context window
	KindCanceled                     // the caller went away
)

func (k ErrKind) String() string {
	switch k {
	case KindRateLimited:
		return "rate_limited"
	case KindServer:
		return "server"
	case KindTimeout:
		return "timeout"
	case KindBadRequest:
		return "bad_request"
	case KindAuth:
		return "auth"
	case KindContextLength:
		return "context_length"
	case KindCanceled:
		return "canceled"
	}
	return "unknown"
}

type ProviderError struct {
	Kind       ErrKind
	Status     int
	RetryAfter time.Duration
	Err        error
}

func (e *ProviderError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("provider error (%s, status %d): %v", e.Kind, e.Status, e.Err)
	}
	return fmt.Sprintf("provider error (%s): %v", e.Kind, e.Err)
}

func (e *ProviderError) Unwrap() error { return e.Err }

// KindOf returns the kind of a provider error. Errors that are not ProviderErrors count as KindServer.
func KindOf(err error) ErrKind {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return KindServer
}

// ClassifyStatus maps an upstream HTTP status to a ProviderError. contextLength is set by the adapter
// when the error body says the prompt was too long for the model.
func ClassifyStatus(status int, retryAfterHeader string, contextLength bool, msg string) *ProviderError {
	pe := &ProviderError{Status: status, Err: errors.New(msg)}
	switch {
	case status == 429:
		pe.Kind = KindRateLimited
		pe.RetryAfter = ParseRetryAfter(retryAfterHeader)
	case status == 401 || status == 403:
		pe.Kind = KindAuth
	case status == 408:
		pe.Kind = KindTimeout
	case contextLength:
		pe.Kind = KindContextLength
	case status >= 500:
		pe.Kind = KindServer
	default:
		pe.Kind = KindBadRequest
	}
	return pe
}

// ParseRetryAfter reads a Retry-After header given in seconds. HTTP dates are not used by the
// providers we talk to and yield zero.
func ParseRetryAfter(v string) time.Duration {
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// WrapTransport converts a transport-level error into a ProviderError.
func WrapTransport(err error) *ProviderError {
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return &ProviderError{Kind: KindCanceled, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &ProviderError{Kind: KindTimeout, Err: err}
	case errors.As(err, &ne) && ne.Timeout():
		return &ProviderError{Kind: KindTimeout, Err: err}
	}
	return &ProviderError{Kind: KindServer, Err: err}
}
