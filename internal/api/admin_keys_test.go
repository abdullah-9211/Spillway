package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/keys"
	"github.com/abdullah-9211/spillway/internal/money"
)

// memKeys is an in-memory KeyAdmin that applies the same validation rules as the real store.
type memKeys struct {
	mu    sync.Mutex
	keys  []keys.Key
	spend map[uuid.UUID]money.Micros
	n     int
}

func newMemKeys() *memKeys { return &memKeys{spend: map[uuid.UUID]money.Micros{}} }

func (m *memKeys) ListWithStats(context.Context, time.Time) ([]keys.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []keys.Stats
	for _, k := range m.keys {
		out = append(out, keys.Stats{Key: k, Spend: m.spend[k.ID]})
	}
	return out, nil
}

func (m *memKeys) Create(_ context.Context, p keys.CreateParams) (keys.Key, string, error) {
	if err := keys.ValidateName(p.Name); err != nil {
		return keys.Key{}, "", err
	}
	if err := keys.ValidateLimits(p.RateLimitRPM, p.MonthlyBudget); err != nil {
		return keys.Key{}, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.n++
	secret := fmt.Sprintf("spw_%048d", m.n)
	k := keys.Key{ID: uuid.New(), Name: p.Name, Prefix: secret[:8], RateLimitRPM: p.RateLimitRPM, MonthlyBudget: p.MonthlyBudget,
		SemanticCache: p.SemanticCache, CacheNonzeroTemp: p.CacheNonzeroTemp, CreatedAt: time.Now()}
	m.keys = append(m.keys, k)
	return k, secret, nil
}

func (m *memKeys) find(id uuid.UUID) (*keys.Key, error) {
	for i := range m.keys {
		if m.keys[i].ID == id {
			return &m.keys[i], nil
		}
	}
	return nil, keys.ErrNotFound
}

func (m *memKeys) Update(_ context.Context, id uuid.UUID, p keys.UpdateParams) (keys.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, err := m.find(id)
	if err != nil {
		return keys.Key{}, err
	}
	if k.RevokedAt != nil {
		return keys.Key{}, keys.ErrRevoked
	}
	if p.Name != nil {
		if err := keys.ValidateName(*p.Name); err != nil {
			return keys.Key{}, err
		}
		k.Name = strings.TrimSpace(*p.Name)
	}
	if err := keys.ValidateLimits(p.RateLimitRPM.Value, p.MonthlyBudget.Value); err != nil {
		return keys.Key{}, err
	}
	if p.RateLimitRPM.Set {
		k.RateLimitRPM = p.RateLimitRPM.Value
	}
	if p.MonthlyBudget.Set {
		k.MonthlyBudget = p.MonthlyBudget.Value
	}
	if p.SemanticCache != nil {
		k.SemanticCache = *p.SemanticCache
	}
	if p.CacheNonzeroTemp != nil {
		k.CacheNonzeroTemp = *p.CacheNonzeroTemp
	}
	return *k, nil
}

func (m *memKeys) RevokeByID(_ context.Context, id uuid.UUID) (keys.Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, err := m.find(id)
	if err != nil {
		return keys.Key{}, err
	}
	if k.RevokedAt == nil {
		now := time.Now().UTC()
		k.RevokedAt = &now
	}
	return *k, nil
}

func newKeysRig(t *testing.T) (*adminRig, *memKeys, string, string) {
	t.Helper()
	r := newAdminRig(t)
	return r, r.keys, r.token(t, "admin", "admin-password"), r.token(t, "viewer", "viewer-password")
}

func (r *adminRig) json(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	resp := r.do(t, method, path, token, body)
	return resp.StatusCode, decode(t, resp)
}

func TestCreateKeyShowsTheSecretOnce(t *testing.T) {
	r, _, admin, viewer := newKeysRig(t)

	code, body := r.json(t, "POST", "/admin/keys", admin, `{"name":"ci-pipeline","rate_limit_rpm":60,"monthly_budget_usd":"30","semantic_cache":true}`)
	if code != 201 {
		t.Fatalf("%d %v", code, body)
	}
	secret, _ := body["secret"].(string)
	key := body["key"].(map[string]any)
	if !strings.HasPrefix(secret, "spw_") || key["prefix"] != secret[:8] || key["name"] != "ci-pipeline" {
		t.Errorf("secret %q key %v", secret, key)
	}
	if key["monthly_budget_usd"] != "30.000000" || key["rate_limit_rpm"].(float64) != 60 || key["semantic_cache"] != true || key["cache_nonzero_temp"] != false {
		t.Errorf("settings did not round trip: %v", key)
	}
	if _, has := key["secret"]; has {
		t.Error("the key object itself must not carry the secret")
	}

	// Neither role can read the secret again, from the list or anywhere else.
	for name, tok := range map[string]string{"admin": admin, "viewer": viewer} {
		code, list := r.json(t, "GET", "/admin/keys", tok, "")
		raw, _ := json.Marshal(list)
		if code != 200 || strings.Contains(string(raw), secret) || strings.Contains(string(raw), `"secret"`) {
			t.Errorf("%s: list leaks the secret or fails: %d %s", name, code, raw)
		}
	}
}

func TestBudgetAcceptsStringsAndNumbers(t *testing.T) {
	r, _, admin, _ := newKeysRig(t)
	for in, want := range map[string]string{`"30"`: "30.000000", `30`: "30.000000", `"0.5"`: "0.500000", `12.34`: "12.340000", `0`: "0.000000", `null`: ""} {
		code, body := r.json(t, "POST", "/admin/keys", admin, `{"name":"k","monthly_budget_usd":`+in+`}`)
		if code != 201 {
			t.Fatalf("%s: %d %v", in, code, body)
		}
		got, _ := body["key"].(map[string]any)["monthly_budget_usd"].(string)
		if got != want {
			t.Errorf("budget %s -> %q, want %q", in, got, want)
		}
	}
}

func TestCreateKeyValidation(t *testing.T) {
	r, k, admin, _ := newKeysRig(t)
	bad := map[string]string{
		"no name": `{}`, "blank name": `{"name":"   "}`, "reserved name": `{"name":"Playground"}`,
		"long name": `{"name":"` + strings.Repeat("x", 65) + `"}`, "zero rpm": `{"name":"a","rate_limit_rpm":0}`,
		"huge rpm": `{"name":"a","rate_limit_rpm":99999999}`, "negative budget": `{"name":"a","monthly_budget_usd":-1}`,
		"text budget": `{"name":"a","monthly_budget_usd":"lots"}`, "too precise": `{"name":"a","monthly_budget_usd":"0.0000001"}`,
		"huge budget": `{"name":"a","monthly_budget_usd":2000000}`, "unknown field": `{"name":"a","secret":"x"}`, "not json": `{`,
		"wrong type": `{"name":1}`,
	}
	for name, body := range bad {
		code, resp := r.json(t, "POST", "/admin/keys", admin, body)
		if code != 400 || resp["error"].(map[string]any)["code"] != "invalid_request" {
			t.Errorf("%s: %d %v", name, code, resp)
		}
	}
	if len(k.keys) != 0 {
		t.Errorf("%d keys were created by invalid requests", len(k.keys))
	}
}

func TestUpdateKey(t *testing.T) {
	r, _, admin, _ := newKeysRig(t)
	_, created := r.json(t, "POST", "/admin/keys", admin, `{"name":"before","rate_limit_rpm":60,"monthly_budget_usd":30}`)
	id := created["key"].(map[string]any)["id"].(string)
	path := "/admin/keys/" + id

	code, k := r.json(t, "PATCH", path, admin, `{"monthly_budget_usd":"50.25","semantic_cache":true}`)
	if code != 200 || k["monthly_budget_usd"] != "50.250000" || k["semantic_cache"] != true || k["rate_limit_rpm"].(float64) != 60 || k["name"] != "before" {
		t.Fatalf("a field left out must not change: %d %v", code, k)
	}
	code, k = r.json(t, "PATCH", path, admin, `{"name":"after","rate_limit_rpm":null}`)
	if code != 200 || k["name"] != "after" || k["rate_limit_rpm"] != nil || k["monthly_budget_usd"] != "50.250000" {
		t.Fatalf("null removes a limit: %d %v", code, k)
	}
	code, k = r.json(t, "PATCH", path, admin, `{"monthly_budget_usd":null,"cache_nonzero_temp":true}`)
	if code != 200 || k["monthly_budget_usd"] != nil || k["cache_nonzero_temp"] != true {
		t.Fatalf("%d %v", code, k)
	}

	for name, body := range map[string]string{"bad rpm": `{"rate_limit_rpm":"fast"}`, "zero rpm": `{"rate_limit_rpm":0}`, "empty name": `{"name":""}`, "bad budget": `{"monthly_budget_usd":"x"}`, "unknown field": `{"revoked_at":null}`} {
		if code, _ := r.json(t, "PATCH", path, admin, body); code != 400 {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code, _ := r.json(t, "PATCH", "/admin/keys/"+uuid.NewString(), admin, `{"name":"x"}`); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
	if code, _ := r.json(t, "PATCH", "/admin/keys/not-a-uuid", admin, `{"name":"x"}`); code != 404 {
		t.Errorf("malformed id: %d", code)
	}
}

func TestRevokeKey(t *testing.T) {
	r, _, admin, _ := newKeysRig(t)
	_, created := r.json(t, "POST", "/admin/keys", admin, `{"name":"doomed"}`)
	path := "/admin/keys/" + created["key"].(map[string]any)["id"].(string)

	code, k := r.json(t, "DELETE", path, admin, "")
	if code != 200 || k["revoked_at"] == nil {
		t.Fatalf("%d %v", code, k)
	}
	if code, k2 := r.json(t, "DELETE", path, admin, ""); code != 200 || k2["revoked_at"] != k["revoked_at"] {
		t.Errorf("revoking twice must change nothing: %d %v", code, k2)
	}
	code, body := r.json(t, "PATCH", path, admin, `{"name":"zombie"}`)
	if code != 409 || body["error"].(map[string]any)["code"] != "key_revoked" {
		t.Errorf("editing a revoked key: %d %v", code, body)
	}
	if code, _ := r.json(t, "DELETE", "/admin/keys/"+uuid.NewString(), admin, ""); code != 404 {
		t.Errorf("unknown id: %d", code)
	}
}

func TestViewerCanReadButNotChange(t *testing.T) {
	r, k, admin, viewer := newKeysRig(t)
	_, created := r.json(t, "POST", "/admin/keys", admin, `{"name":"shared"}`)
	path := "/admin/keys/" + created["key"].(map[string]any)["id"].(string)

	if code, body := r.json(t, "GET", "/admin/keys", viewer, ""); code != 200 || len(body["keys"].([]any)) != 1 {
		t.Errorf("viewer list: %d %v", code, body)
	}
	for name, c := range map[string]struct{ method, path, body string }{
		"create": {"POST", "/admin/keys", `{"name":"sneaky"}`},
		"edit":   {"PATCH", path, `{"monthly_budget_usd":1000}`},
		"revoke": {"DELETE", path, ""},
	} {
		code, body := r.json(t, c.method, c.path, viewer, c.body)
		if code != 403 || body["error"].(map[string]any)["code"] != "forbidden" {
			t.Errorf("viewer %s: %d %v", name, code, body)
		}
	}
	if len(k.keys) != 1 || k.keys[0].RevokedAt != nil || k.keys[0].MonthlyBudget != nil || k.keys[0].Name != "shared" {
		t.Errorf("a viewer's refused requests changed state: %+v", k.keys)
	}
}

func TestListShowsSpendAndFlags(t *testing.T) {
	r, k, admin, _ := newKeysRig(t)
	_, created := r.json(t, "POST", "/admin/keys", admin, `{"name":"spender","monthly_budget_usd":100}`)
	id := uuid.MustParse(created["key"].(map[string]any)["id"].(string))
	k.spend[id] = 42_100_000

	_, list := r.json(t, "GET", "/admin/keys", admin, "")
	row := list["keys"].([]any)[0].(map[string]any)
	if row["spend_usd"] != "42.100000" || row["builtin"] != false || row["last_used_at"] != nil {
		t.Errorf("row = %v", row)
	}
}
