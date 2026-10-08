package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
)

type memUsers map[string]auth.User

func (m memUsers) Get(_ context.Context, name string) (auth.User, error) {
	if u, ok := m[name]; ok {
		return u, nil
	}
	return auth.User{}, auth.ErrNoUser
}

var testHashParams = auth.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 16, SaltLen: 8}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.Hash(pw, testHashParams)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type adminRig struct {
	usage *fakeUsage
	keys  *memKeys
	admin *Admin
	srv   *httptest.Server
	now   *time.Time
}

func newAdminRig(t *testing.T) *adminRig {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	signer, err := auth.NewSignerWithClock([]byte(strings.Repeat("s", 32)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	users := memUsers{
		"admin":  {ID: uuid.New(), Username: "admin", Hash: mustHash(t, "admin-password"), Role: auth.RoleAdmin},
		"viewer": {ID: uuid.New(), Username: "viewer", Hash: mustHash(t, "viewer-password"), Role: auth.RoleViewer},
	}
	mk := newMemKeys()
	fu := &fakeUsage{}
	a := NewAdmin(AdminDeps{Users: users, Keys: mk, Usage: fu, Health: testHealth, Latency: fakeLatency{ok: true}, Signer: signer, DummyHashParams: testHashParams, Deps: []Dependency{
		{Checker: fakeDep{"postgres", nil}, Required: true}, {Checker: fakeDep{"redis", io.ErrClosedPipe}, Required: false}}})
	r := &adminRig{admin: a, now: &now, keys: mk, usage: fu}
	r.srv = httptest.NewServer(NewHandler(Options{Admin: a}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *adminRig) do(t *testing.T, method, path, token, body string, hdr ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (r *adminRig) login(t *testing.T, user, pw string, hdr ...string) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": user, "password": pw})
	return r.do(t, "POST", "/admin/login", "", string(b), hdr...)
}

func (r *adminRig) token(t *testing.T, user, pw string) string {
	t.Helper()
	resp := r.login(t, user, pw)
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("login %s: status %d err %v", user, resp.StatusCode, err)
	}
	return out.Token
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoginAndMe(t *testing.T) {
	r := newAdminRig(t)
	for user, pw := range map[string]string{"admin": "admin-password", "viewer": "viewer-password"} {
		resp := r.login(t, user, pw)
		body := decode(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", user, resp.StatusCode, body)
		}
		u := body["user"].(map[string]any)
		if u["username"] != user || u["role"] != user {
			t.Errorf("%s: user = %v", user, u)
		}
		exp, err := time.Parse(time.RFC3339, body["expires_at"].(string))
		if err != nil || !exp.Equal(r.now.Add(12*time.Hour)) {
			t.Errorf("%s: expires_at = %v (%v), want 12h from now", user, body["expires_at"], err)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("sign-in responses must not be cached")
		}

		me := r.do(t, "GET", "/admin/me", body["token"].(string), "")
		meBody := decode(t, me)
		if me.StatusCode != 200 || meBody["username"] != user || meBody["role"] != user {
			t.Errorf("%s: /admin/me = %d %v", user, me.StatusCode, meBody)
		}
	}
}

func TestLoginFailuresLookIdentical(t *testing.T) {
	r := newAdminRig(t)
	a := r.login(t, "admin", "wrong")
	b := r.login(t, "nobody", "whatever")
	ba, bb := decode(t, a), decode(t, b)
	if a.StatusCode != 401 || b.StatusCode != 401 {
		t.Fatalf("statuses %d %d", a.StatusCode, b.StatusCode)
	}
	if !jsonEqual(ba, bb) {
		t.Errorf("a wrong password and an unknown user must be indistinguishable:\n%v\n%v", ba, bb)
	}
	if code := ba["error"].(map[string]any)["code"]; code != "invalid_credentials" {
		t.Errorf("code = %v", code)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestLoginRejectsBadBodies(t *testing.T) {
	r := newAdminRig(t)
	for name, body := range map[string]string{"not json": "{", "empty": "{}", "no password": `{"username":"admin"}`, "no username": `{"password":"x"}`, "wrong types": `{"username":1,"password":2}`} {
		resp := r.do(t, "POST", "/admin/login", "", body)
		if resp.StatusCode != 400 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
}

func TestFiveBadLoginsAreThrottled(t *testing.T) {
	r := newAdminRig(t)
	for i := 1; i <= 5; i++ {
		if resp := r.login(t, "admin", "wrong", "X-Forwarded-For", "9.9.9.9"); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	// The sixth is refused even with the right password, until the minute is up.
	resp := r.login(t, "admin", "admin-password", "X-Forwarded-For", "9.9.9.9")
	body := decode(t, resp)
	if resp.StatusCode != 429 || body["error"].(map[string]any)["code"] != "too_many_attempts" {
		t.Fatalf("sixth attempt: %d %v", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q", ra)
	}
	// The username limit holds from another address; an unrelated user from another address is unaffected.
	if resp := r.login(t, "admin", "admin-password", "X-Forwarded-For", "8.8.8.8"); resp.StatusCode != 429 {
		t.Errorf("same username from another address: %d, want 429", resp.StatusCode)
	}
	if resp := r.login(t, "viewer", "viewer-password", "X-Forwarded-For", "7.7.7.7"); resp.StatusCode != 200 {
		t.Errorf("unrelated user and address: %d, want 200", resp.StatusCode)
	}
}

func TestSuccessfulLoginsAreNeverThrottled(t *testing.T) {
	r := newAdminRig(t)
	for i := 0; i < 12; i++ {
		if resp := r.login(t, "admin", "admin-password"); resp.StatusCode != 200 {
			t.Fatalf("login %d: %d", i+1, resp.StatusCode)
		}
	}
}

func TestSessionRejections(t *testing.T) {
	r := newAdminRig(t)
	good := r.token(t, "admin", "admin-password")
	payload, sig, _ := strings.Cut(good, ".")
	viewer := r.token(t, "viewer", "viewer-password")
	vPayload, _, _ := strings.Cut(viewer, ".")

	tests := map[string]string{
		"no token": "", "garbage": "abc.def", "tampered signature": payload + "." + sig[:len(sig)-2] + "AA",
		"viewer payload with admin signature": vPayload + "." + sig,
	}
	for name, tok := range tests {
		resp := r.do(t, "GET", "/admin/me", tok, "")
		if resp.StatusCode != 401 {
			t.Errorf("%s: %d, want 401", name, resp.StatusCode)
		}
	}

	*r.now = r.now.Add(12*time.Hour + time.Second)
	resp := r.do(t, "GET", "/admin/me", good, "")
	body := decode(t, resp)
	if resp.StatusCode != 401 || body["error"].(map[string]any)["code"] != "session_expired" {
		t.Errorf("expired: %d %v", resp.StatusCode, body)
	}
}

func TestStatus(t *testing.T) {
	r := newAdminRig(t)
	resp := r.do(t, "GET", "/admin/status", r.token(t, "viewer", "viewer-password"), "")
	body := decode(t, resp)
	if resp.StatusCode != 200 || body["postgres"] != "up" || body["redis"] != "down" {
		t.Errorf("%d %v", resp.StatusCode, body)
	}
}

func TestNoSecretsInResponses(t *testing.T) {
	r := newAdminRig(t)
	tok := r.token(t, "admin", "admin-password")
	for _, path := range []string{"/admin/me", "/admin/status"} {
		resp := r.do(t, "GET", path, tok, "")
		raw, _ := io.ReadAll(resp.Body)
		for _, secret := range []string{"argon2", "password", "admin-password"} {
			if strings.Contains(strings.ToLower(string(raw)), secret) {
				t.Errorf("%s leaks %q: %s", path, secret, raw)
			}
		}
	}
}

// Unknown usernames must cost about as much as known ones, or response time reveals which usernames exist.
func TestUnknownUserCostsAboutAsMuchAsAWrongPassword(t *testing.T) {
	signer, _ := auth.NewSigner([]byte(strings.Repeat("s", 32)))
	srv := httptest.NewServer(NewHandler(Options{Admin: NewAdmin(AdminDeps{Users: memUsers{}, Signer: signer})})) // default cost
	defer srv.Close()
	start := time.Now()
	resp, err := http.Post(srv.URL+"/admin/login", "application/json", strings.NewReader(`{"username":"no-such-user","password":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	took := time.Since(start)
	h, _ := auth.Hash("x", auth.DefaultParams)
	t0 := time.Now()
	_, _ = auth.Verify("y", h)
	oneVerify := time.Since(t0)
	if took < oneVerify/2 {
		t.Errorf("unknown-user sign-in took %v but one production-cost verification takes %v; it must do the work", took, oneVerify)
	}
}

// --- the role matrix, generated from the router ---

func TestRoleMatrixFromTheRouter(t *testing.T) {
	r := newAdminRig(t)
	// A write route, standing in for the key, budget, tool and approval endpoints later phases add. The point is
	// that the matrix below is built from whatever is registered, so those routes are covered the moment they exist.
	r.admin.handle("POST", "/admin/_matrix/write", AccessAdmin, func(w http.ResponseWriter, _ *http.Request, _ auth.Claims) {
		writeJSON(w, 200, map[string]string{"ok": "yes"})
	})
	r.admin.handle("GET", "/admin/_matrix/read", AccessViewer, func(w http.ResponseWriter, _ *http.Request, _ auth.Claims) {
		writeJSON(w, 200, map[string]string{"ok": "yes"})
	})

	admin := r.token(t, "admin", "admin-password")
	viewer := r.token(t, "viewer", "viewer-password")

	routes := r.admin.Routes()
	if len(routes) < 12 {
		t.Fatalf("expected the real routes plus the two above, got %v", routes)
	}
	// "allowed" means the request got past the session and role checks: whatever the handler then says about a
	// made-up body or id (400, 404) is its own business, but never 401 or 403.
	denied := func(code int) bool { return code == 401 || code == 403 }
	for _, rt := range routes {
		if rt.Access == AccessPublic {
			continue // login has its own tests
		}
		path := strings.ReplaceAll(rt.Path, "{id}", uuid.NewString())
		status := func(token string) int { return r.do(t, rt.Method, path, token, "{}").StatusCode }
		if got := status(""); got != 401 {
			t.Errorf("%s %s: no token gave %d, want 401", rt.Method, rt.Path, got)
		}
		if got := status(admin[:len(admin)-3] + "AAA"); got != 401 {
			t.Errorf("%s %s: tampered token gave %d, want 401", rt.Method, rt.Path, got)
		}
		viewerCode, adminCode := status(viewer), status(admin)
		if denied(adminCode) {
			t.Errorf("%s %s (%s): an admin was refused with %d", rt.Method, rt.Path, rt.Access, adminCode)
		}
		switch rt.Access {
		case AccessViewer:
			if denied(viewerCode) {
				t.Errorf("%s %s: a viewer was refused with %d", rt.Method, rt.Path, viewerCode)
			}
		case AccessAdmin:
			if viewerCode != 403 {
				t.Errorf("%s %s: a viewer got %d, want 403", rt.Method, rt.Path, viewerCode)
			}
		}
	}

	// A viewer's 403 says why and never reveals more.
	resp := r.do(t, "POST", "/admin/_matrix/write", viewer, "{}")
	body := decode(t, resp)
	if body["error"].(map[string]any)["code"] != "forbidden" {
		t.Errorf("403 body = %v", body)
	}
}

func TestProductionRoutesAreAllDeclared(t *testing.T) {
	r := newAdminRig(t)
	var got []string
	for _, rt := range r.admin.Routes() {
		got = append(got, rt.Method+" "+rt.Path+" "+rt.Access.String())
	}
	sort.Strings(got)
	want := []string{
		"DELETE /admin/keys/{id} admin", "GET /admin/keys viewer", "GET /admin/me viewer", "GET /admin/status viewer",
		"GET /admin/usage/export.csv viewer", "GET /admin/usage/requests viewer", "GET /admin/usage/summary viewer",
		"PATCH /admin/keys/{id} admin", "POST /admin/keys admin", "POST /admin/login public",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("routes = %v, want %v", got, want)
	}
}
