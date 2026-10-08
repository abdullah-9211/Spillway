package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/gateway"
)

// Access says who may call an admin route. Every route is registered with one, which is what the role
// matrix test walks, so a new route cannot be added without deciding who may use it.
type Access int

const (
	AccessPublic Access = iota // no session needed (login)
	AccessViewer               // any signed-in user, so admin and viewer
	AccessAdmin                // admin only; a viewer gets 403
)

func (a Access) String() string { return [...]string{"public", "viewer", "admin"}[a] }

// Route is one registered admin endpoint.
type Route struct {
	Method string
	Path   string
	Access Access
}

// UserStore finds the account a sign-in is for.
type UserStore interface {
	Get(ctx context.Context, username string) (auth.User, error)
}

// AdminDeps are what the admin API needs. A nil Signer or Users leaves the admin API off.
type AdminDeps struct {
	Users    UserStore
	Signer   *auth.Signer
	Throttle *auth.Throttle
	Keys     KeyAdmin   // the key endpoints are registered only when this is set
	Usage    UsageAdmin // the usage endpoints are registered only when this is set
	// Playground, when set, serves /admin/models and /admin/playground/*.
	Playground *PlaygroundDeps
	// Health and Latency feed the provider-health and added-latency parts of the usage summary.
	Health  func() []gateway.ProviderHealth
	Latency LatencySource
	Deps    []Dependency // reported by /admin/status
	// DummyHashParams is the cost of the hash checked for unknown usernames. It must match the cost of the
	// real hashes, or timing tells an attacker which usernames exist. Zero means auth.DefaultParams.
	DummyHashParams auth.Params
	Log             *slog.Logger
}

// Admin is the dashboard's API, mounted under /admin. Its caller is the dashboard's server-side layer, which
// forwards the session token as a Bearer token.
type Admin struct {
	d         AdminDeps
	mux       *http.ServeMux
	routes    []Route
	dummyHash string // verified against when the username is unknown, so timing does not reveal which usernames exist
}

type claimsHandler func(w http.ResponseWriter, r *http.Request, c auth.Claims)

func NewAdmin(d AdminDeps) *Admin {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Throttle == nil {
		d.Throttle = auth.NewThrottle(5, time.Minute)
	}
	a := &Admin{d: d, mux: http.NewServeMux()}
	hp := d.DummyHashParams
	if hp.Memory == 0 {
		hp = auth.DefaultParams
	}
	a.dummyHash, _ = auth.Hash("not-a-real-password", hp)

	a.public("POST", "/admin/login", a.login)
	a.handle("GET", "/admin/me", AccessViewer, a.me)
	a.handle("GET", "/admin/status", AccessViewer, a.status)
	if d.Keys != nil {
		a.registerKeys()
	}
	if d.Usage != nil {
		a.registerUsage()
	}
	if d.Playground != nil {
		a.registerPlayground()
	}
	return a
}

func (a *Admin) Handler() http.Handler { return a.mux }

// Routes lists every registered route with its access level.
func (a *Admin) Routes() []Route { return append([]Route(nil), a.routes...) }

func (a *Admin) public(method, path string, h http.HandlerFunc) {
	a.routes = append(a.routes, Route{method, path, AccessPublic})
	a.mux.HandleFunc(method+" "+path, h)
}

// handle registers a route behind the session check and, for AccessAdmin, the role check.
func (a *Admin) handle(method, path string, access Access, h claimsHandler) {
	a.routes = append(a.routes, Route{method, path, access})
	a.mux.HandleFunc(method+" "+path, func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.session(w, r)
		if !ok {
			return
		}
		if access == AccessAdmin && c.Role != auth.RoleAdmin {
			writeAdminError(w, http.StatusForbidden, "forbidden", "This needs the admin role. Viewers can read but not change.")
			return
		}
		h(w, r, c)
	})
}

func (a *Admin) session(w http.ResponseWriter, r *http.Request) (auth.Claims, bool) {
	token, ok := bearer(r)
	if !ok {
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return auth.Claims{}, false
	}
	c, err := a.d.Signer.Verify(token)
	switch {
	case errors.Is(err, auth.ErrExpiredToken):
		writeAdminError(w, http.StatusUnauthorized, "session_expired", "Your session has expired. Sign in again.")
		return auth.Claims{}, false
	case err != nil:
		writeAdminError(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue.")
		return auth.Claims{}, false
	}
	return c, true
}

type adminErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeAdminError(w http.ResponseWriter, status int, code, msg string) {
	var b adminErrorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- handlers ---

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type userBody struct {
	Username string    `json:"username"`
	Role     auth.Role `json:"role"`
}

type loginResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	User      userBody  `json:"user"`
}

// clientIP is the address the sign-in came from. The dashboard's server makes the call, so the connection
// itself is always the dashboard; it passes the browser's address in X-Forwarded-For. The admin API is
// reachable only from the dashboard, which is why that header is trusted. If it were ever exposed, a caller
// could spoof the address, but the per-username limit would still hold.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "Send a JSON body with a username and a password.")
		return
	}
	ip := clientIP(r)
	if blocked, wait := a.d.Throttle.Blocked(ip, req.Username); blocked {
		secs := max(int((wait+time.Second-1)/time.Second), 1)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeAdminError(w, http.StatusTooManyRequests, "too_many_attempts", "Too many failed sign-ins. Try again in a minute.")
		return
	}

	user, err := a.d.Users.Get(r.Context(), req.Username)
	hash := user.Hash
	switch {
	case errors.Is(err, auth.ErrNoUser):
		hash = a.dummyHash
	case err != nil:
		a.d.Log.Error("sign-in lookup failed", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not check your sign-in. Try again.")
		return
	}
	ok, verr := auth.Verify(req.Password, hash)
	if verr != nil {
		a.d.Log.Error("stored password hash is malformed", "username", req.Username, "error", verr)
	}
	if err != nil || verr != nil || !ok {
		a.d.Throttle.Fail(ip, req.Username)
		writeAdminError(w, http.StatusUnauthorized, "invalid_credentials", "That username and password do not match.")
		return
	}

	a.d.Throttle.Reset(req.Username)
	token, exp, err := a.d.Signer.Issue(auth.Claims{UserID: user.ID.String(), Username: user.Username, Role: user.Role})
	if err != nil {
		a.d.Log.Error("could not issue a session token", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not sign you in. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, loginResponse{Token: token, ExpiresAt: exp, User: userBody{Username: user.Username, Role: user.Role}})
}

func (a *Admin) me(w http.ResponseWriter, _ *http.Request, c auth.Claims) {
	writeJSON(w, http.StatusOK, userBody{Username: c.Username, Role: c.Role})
}

type statusBody struct {
	Postgres string `json:"postgres"`
	Redis    string `json:"redis"` // up, down, or disabled when Redis is not configured
}

func (a *Admin) status(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	out := statusBody{Postgres: "unknown", Redis: "disabled"}
	for _, d := range a.d.Deps {
		state := "up"
		if err := d.Checker.Ping(ctx); err != nil {
			state = "down"
		}
		switch d.Checker.Name() {
		case "postgres":
			out.Postgres = state
		case "redis":
			out.Redis = state
		}
	}
	writeJSON(w, http.StatusOK, out)
}
