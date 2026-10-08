package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
)

// KeyAdmin is what the key endpoints need from the key store.
type KeyAdmin interface {
	ListWithStats(ctx context.Context, now time.Time) ([]keys.Stats, error)
	Create(ctx context.Context, p keys.CreateParams) (keys.Key, string, error)
	Update(ctx context.Context, id uuid.UUID, p keys.UpdateParams) (keys.Key, error)
	RevokeByID(ctx context.Context, id uuid.UUID) (keys.Key, error)
}

func (a *Admin) registerKeys() {
	a.handle("GET", "/admin/keys", AccessViewer, a.listKeys)
	a.handle("POST", "/admin/keys", AccessAdmin, a.createKey)
	a.handle("PATCH", "/admin/keys/{id}", AccessAdmin, a.updateKey)
	a.handle("DELETE", "/admin/keys/{id}", AccessAdmin, a.revokeKey)
}

// keyBody is a key as the dashboard sees it. It never contains the key itself, only its prefix, so a viewer
// cannot learn a secret. Money is a decimal string (dollars, six places), never a float.
type keyBody struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Prefix           string     `json:"prefix"`
	RateLimitRPM     *int       `json:"rate_limit_rpm"`
	MonthlyBudgetUSD *string    `json:"monthly_budget_usd"`
	SpendUSD         string     `json:"spend_usd"`
	SemanticCache    bool       `json:"semantic_cache"`
	CacheNonzeroTemp bool       `json:"cache_nonzero_temp"`
	CreatedAt        time.Time  `json:"created_at"`
	RevokedAt        *time.Time `json:"revoked_at"`
	LastUsedAt       *time.Time `json:"last_used_at"`
	Builtin          bool       `json:"builtin"`
}

func toKeyBody(k keys.Key, spend money.Micros, lastUsed *time.Time) keyBody {
	b := keyBody{
		ID: k.ID.String(), Name: k.Name, Prefix: k.Prefix, RateLimitRPM: k.RateLimitRPM, SpendUSD: spend.String(),
		SemanticCache: k.SemanticCache, CacheNonzeroTemp: k.CacheNonzeroTemp, CreatedAt: k.CreatedAt.UTC(),
		RevokedAt: k.RevokedAt, LastUsedAt: lastUsed, Builtin: k.Name == keys.BuiltinName,
	}
	if k.MonthlyBudget != nil {
		s := k.MonthlyBudget.String()
		b.MonthlyBudgetUSD = &s
	}
	return b
}

func (a *Admin) listKeys(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	list, err := a.d.Keys.ListWithStats(r.Context(), time.Now())
	if err != nil {
		a.d.Log.Error("list keys", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the keys.")
		return
	}
	out := make([]keyBody, 0, len(list))
	for _, s := range list {
		out = append(out, toKeyBody(s.Key, s.Spend, s.LastUsedAt))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// dollars reads a JSON amount that may be a string ("30.50") or a number (30.5) without going through a float.
func dollars(raw json.RawMessage) (*money.Micros, error) {
	t := strings.TrimSpace(string(raw))
	if t == "null" || t == "" {
		return nil, nil
	}
	t = strings.Trim(t, `"`)
	m, err := money.ParseUSD(t)
	if err != nil {
		return nil, errors.New("the budget must be a dollar amount such as 30 or 30.50")
	}
	return &m, nil
}

func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "The request body is not valid: "+err.Error())
		return false
	}
	return true
}

// keyError maps a store error to a response.
func (a *Admin) keyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, keys.ErrInvalid):
		writeAdminError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), keys.ErrInvalid.Error()+": "))
	case errors.Is(err, keys.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such key.")
	case errors.Is(err, keys.ErrRevoked):
		writeAdminError(w, http.StatusConflict, "key_revoked", "A revoked key cannot be changed.")
	default:
		a.d.Log.Error("key admin", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Something went wrong. Try again.")
	}
}

type createKeyRequest struct {
	Name             string          `json:"name"`
	RateLimitRPM     *int            `json:"rate_limit_rpm"`
	MonthlyBudgetUSD json.RawMessage `json:"monthly_budget_usd"`
	SemanticCache    bool            `json:"semantic_cache"`
	CacheNonzeroTemp bool            `json:"cache_nonzero_temp"`
}

func (a *Admin) createKey(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	var req createKeyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	budget, err := dollars(req.MonthlyBudgetUSD)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	k, secret, err := a.d.Keys.Create(r.Context(), keys.CreateParams{
		Name: strings.TrimSpace(req.Name), RateLimitRPM: req.RateLimitRPM, MonthlyBudget: budget,
		SemanticCache: req.SemanticCache, CacheNonzeroTemp: req.CacheNonzeroTemp,
	})
	if err != nil {
		a.keyError(w, err)
		return
	}
	// The one and only time the full key leaves the server.
	writeJSON(w, http.StatusCreated, map[string]any{"key": toKeyBody(k, 0, nil), "secret": secret})
}

type updateKeyRequest struct {
	Name             *string         `json:"name"`
	RateLimitRPM     json.RawMessage `json:"rate_limit_rpm"`
	MonthlyBudgetUSD json.RawMessage `json:"monthly_budget_usd"`
	SemanticCache    *bool           `json:"semantic_cache"`
	CacheNonzeroTemp *bool           `json:"cache_nonzero_temp"`
}

func (a *Admin) updateKey(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such key.")
		return
	}
	var req updateKeyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	p := keys.UpdateParams{Name: req.Name, SemanticCache: req.SemanticCache, CacheNonzeroTemp: req.CacheNonzeroTemp}
	if req.RateLimitRPM != nil { // present: a number sets the limit, null removes it
		p.RateLimitRPM.Set = true
		if !bytes.Equal(bytes.TrimSpace(req.RateLimitRPM), []byte("null")) {
			var n int
			if err := json.Unmarshal(req.RateLimitRPM, &n); err != nil {
				writeAdminError(w, http.StatusBadRequest, "invalid_request", "The rate limit must be a whole number, or null for no limit.")
				return
			}
			p.RateLimitRPM.Value = &n
		}
	}
	if req.MonthlyBudgetUSD != nil {
		p.MonthlyBudget.Set = true
		if p.MonthlyBudget.Value, err = dollars(req.MonthlyBudgetUSD); err != nil {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
	}
	k, err := a.d.Keys.Update(r.Context(), id, p)
	if err != nil {
		a.keyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toKeyBody(k, 0, nil))
}

func (a *Admin) revokeKey(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such key.")
		return
	}
	k, err := a.d.Keys.RevokeByID(r.Context(), id)
	if err != nil {
		a.keyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toKeyBody(k, 0, nil))
}
