package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/gateway"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/provider"
	"github.com/abdullah-9211/spillway/internal/usage"
)

const maxBodyBytes = 10 << 20

// writeTimeout is the per-write deadline on SSE responses; a client stalled longer is treated as gone.
// A variable so tests can shorten it.
var writeTimeout = 10 * time.Second

type v1 struct {
	gw   *gateway.Gateway
	auth Authenticator
	log  *slog.Logger
}

type authedHandler func(w http.ResponseWriter, r *http.Request, key keys.Key, id uuid.UUID)

// withRequest assigns the request id, authenticates, and calls h.
func (v *v1) withRequest(h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.NewV7()
		if err != nil {
			id = uuid.New()
		}
		w.Header().Set("X-Spillway-Request-Id", id.String())

		token, ok := bearer(r)
		if !ok {
			writeError(w, 401, "authentication_error", "invalid_api_key", "", "Missing API key. Send it as 'Authorization: Bearer <key>'.")
			return
		}
		key, err := v.auth.Authenticate(r.Context(), token)
		switch {
		case errors.Is(err, keys.ErrInvalidKey):
			writeError(w, 401, "authentication_error", "invalid_api_key", "", "Invalid API key.")
			return
		case err != nil:
			v.log.Error("key lookup failed", "request_id", id, "error", err)
			writeError(w, 500, "api_error", "internal_error", "", "Could not verify the API key.")
			return
		}
		h(w, r, key, id)
	})
}

func bearer(r *http.Request) (string, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

type errorBody struct {
	Error struct {
		Message  string          `json:"message"`
		Type     string          `json:"type"`
		Code     string          `json:"code"`
		Param    *string         `json:"param"`
		Attempts []usage.Attempt `json:"attempts,omitempty"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, typ, code, param, msg string) {
	writeErrorWithAttempts(w, status, typ, code, param, msg, nil)
}

func writeErrorWithAttempts(w http.ResponseWriter, status int, typ, code, param, msg string, attempts []usage.Attempt) {
	var b errorBody
	b.Error.Message, b.Error.Type, b.Error.Code, b.Error.Attempts = msg, typ, code, attempts
	if param != "" {
		b.Error.Param = &param
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

func writeGatewayError(w http.ResponseWriter, err error) {
	var ge *gateway.Error
	if errors.As(err, &ge) {
		if ge.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(ge.RetryAfter.Round(time.Second).Seconds())))
		}
		writeErrorWithAttempts(w, ge.Status, ge.Type, ge.Code, ge.Param, ge.Message, ge.Attempts)
		return
	}
	writeError(w, 500, "api_error", "internal_error", "", "Internal error.")
}

func (v *v1) chatCompletions(w http.ResponseWriter, r *http.Request, key keys.Key, id uuid.UUID) {
	var req provider.ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, 413, "invalid_request_error", "request_too_large", "", "Request body is too large.")
			return
		}
		writeError(w, 400, "invalid_request_error", "invalid_request", "", "Could not parse the request body as JSON: "+err.Error())
		return
	}
	if req.Stream {
		v.stream(w, r, key, id, &req)
		return
	}

	res, err := v.gw.Chat(r.Context(), key, id, r.Header.Get("X-Spillway-Bucket"), &req)
	if err != nil {
		writeGatewayError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-Spillway-Provider", res.Provider)
	h.Set("X-Spillway-Model", res.Model)
	h.Set("X-Spillway-Cache", cacheHeader(res.Cache))
	h.Set("X-Spillway-Attempts", fmt.Sprint(res.Attempts))
	h.Set("X-Spillway-Cost-Usd", res.Cost.String())
	_ = json.NewEncoder(w).Encode(res.Response)
}

func (v *v1) stream(w http.ResponseWriter, r *http.Request, key keys.Key, id uuid.UUID, req *provider.ChatRequest) {
	ctx := r.Context()
	st, err := v.gw.ChatStream(ctx, key, id, r.Header.Get("X-Spillway-Bucket"), req)
	if err != nil {
		writeGatewayError(w, err)
		return
	}

	// Nothing has been sent yet, so a failure on the first read can still be a plain HTTP error.
	first, err := st.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("the provider returned an empty stream")
		}
		st.Finish(ctx, err)
		if ctx.Err() == nil {
			writeError(w, 502, "api_error", "upstream_error", "", "The provider failed before sending any data: "+err.Error())
		}
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Spillway-Provider", st.Provider())
	h.Set("X-Spillway-Model", st.Model())
	h.Set("X-Spillway-Cache", cacheHeader(st.Cache()))
	h.Set("X-Spillway-Attempts", fmt.Sprint(st.Attempts()))
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	send := func(payload string) error {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		return rc.Flush()
	}
	sendChunk := func(c *provider.ChatChunk) error {
		b, err := json.Marshal(c)
		if err != nil {
			return err
		}
		return send(string(b))
	}

	chunk := first
	for {
		if err := sendChunk(chunk); err != nil {
			// The client is gone or too slow. Finish closes the upstream stream, which cancels its request.
			st.Finish(ctx, fmt.Errorf("client write failed: %w", &provider.ProviderError{Kind: provider.KindCanceled, Err: err}))
			return
		}
		chunk, err = st.Next()
		if errors.Is(err, io.EOF) {
			_ = send("[DONE]")
			st.Finish(ctx, nil)
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				// Bytes are already out, so we cannot switch provider: say what happened and end the stream.
				b, _ := json.Marshal(gateway.ErrorPayload("The provider failed mid-stream: " + err.Error()))
				_ = send(string(b))
				_ = send("[DONE]")
			}
			st.Finish(ctx, err)
			return
		}
	}
}

type modelList struct {
	Object string      `json:"object"`
	Data   []modelItem `json:"data"`
}

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (v *v1) models(w http.ResponseWriter, r *http.Request, _ keys.Key, _ uuid.UUID) {
	cat := v.gw.Catalog()
	models, policies := cat.Names()
	out := modelList{Object: "list", Data: []modelItem{}}
	for _, id := range models {
		out.Data = append(out.Data, modelItem{ID: id, Object: "model", OwnedBy: cat.Models[id].Provider})
	}
	for _, p := range policies {
		out.Data = append(out.Data, modelItem{ID: p, Object: "model", OwnedBy: "spillway"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// cacheHeader renders the cache status the way the X-Spillway-Cache header documents it.
func cacheHeader(s usage.CacheStatus) string {
	switch s {
	case usage.CacheHitExact:
		return "hit-exact"
	case usage.CacheHitSemantic:
		return "hit-semantic"
	case usage.CacheMiss:
		return "miss"
	}
	return "bypass"
}
