package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/google/uuid"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("api/openapi.yaml is not valid OpenAPI: %v", err)
	}
	return doc
}

// The spec and the router must describe the same endpoints with the same access levels.
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	doc := loadSpec(t)
	var fromSpec, fromRouter []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			access, _ := op.Extensions["x-access"].(string)
			if access == "" {
				// kin-openapi stores extensions as raw JSON
				if raw, ok := op.Extensions["x-access"].(json.RawMessage); ok {
					_ = json.Unmarshal(raw, &access)
				}
			}
			fromSpec = append(fromSpec, method+" "+path+" "+access)
		}
	}
	for _, rt := range newAdminRig(t).admin.Routes() {
		fromRouter = append(fromRouter, rt.Method+" "+rt.Path+" "+rt.Access.String())
	}
	sort.Strings(fromSpec)
	sort.Strings(fromRouter)
	if strings.Join(fromSpec, "|") != strings.Join(fromRouter, "|") {
		t.Errorf("api/openapi.yaml and the admin router disagree.\nspec:   %v\nrouter: %v", fromSpec, fromRouter)
	}
}

// Real responses, checked against the schema, so the web app and the Go service cannot drift apart.
func TestResponsesMatchTheOpenAPISchema(t *testing.T) {
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	r := newAdminRig(t)
	admin := r.token(t, "admin", "admin-password")
	viewer := r.token(t, "viewer", "viewer-password")

	_, created := r.json(t, "POST", "/admin/keys", admin, `{"name":"seed-key"}`)
	keyID := created["key"].(map[string]any)["id"].(string)

	// Throttle a username so a 429 is on the list too.
	for i := 0; i < 5; i++ {
		r.login(t, "throttled", "x", "X-Forwarded-For", "4.4.4.4")
	}

	cases := []struct {
		name         string
		method, path string
		token, body  string
		hdr          []string
		wantStatus   int
	}{
		{"login ok", "POST", "/admin/login", "", `{"username":"admin","password":"admin-password"}`, nil, 200},
		{"login viewer", "POST", "/admin/login", "", `{"username":"viewer","password":"viewer-password"}`, nil, 200},
		{"login bad body", "POST", "/admin/login", "", `{`, nil, 400},
		{"login bad password", "POST", "/admin/login", "", `{"username":"admin","password":"nope"}`, []string{"X-Forwarded-For", "3.3.3.3"}, 401},
		{"login throttled", "POST", "/admin/login", "", `{"username":"throttled","password":"x"}`, []string{"X-Forwarded-For", "4.4.4.4"}, 429},
		{"me admin", "GET", "/admin/me", admin, "", nil, 200},
		{"me viewer", "GET", "/admin/me", viewer, "", nil, 200},
		{"me without a token", "GET", "/admin/me", "", "", nil, 401},
		{"status", "GET", "/admin/status", viewer, "", nil, 200},
		{"status without a token", "GET", "/admin/status", "", "", nil, 401},
		{"list keys", "GET", "/admin/keys", viewer, "", nil, 200},
		{"list keys without a token", "GET", "/admin/keys", "", "", nil, 401},
		{"create key", "POST", "/admin/keys", admin, `{"name":"contract","rate_limit_rpm":60,"monthly_budget_usd":"30.50","semantic_cache":true}`, nil, 201},
		{"create key with a numeric budget", "POST", "/admin/keys", admin, `{"name":"contract2","monthly_budget_usd":12}`, nil, 201},
		{"create key invalid", "POST", "/admin/keys", admin, `{"name":""}`, nil, 400},
		{"create key as viewer", "POST", "/admin/keys", viewer, `{"name":"nope"}`, nil, 403},
		{"update key", "PATCH", "/admin/keys/" + keyID, admin, `{"monthly_budget_usd":null,"name":"renamed"}`, nil, 200},
		{"update unknown key", "PATCH", "/admin/keys/" + uuid.NewString(), admin, `{"name":"x"}`, nil, 404},
		{"update as viewer", "PATCH", "/admin/keys/" + keyID, viewer, `{"name":"x"}`, nil, 403},
		{"revoke key", "DELETE", "/admin/keys/" + keyID, admin, "", nil, 200},
		{"update revoked key", "PATCH", "/admin/keys/" + keyID, admin, `{"name":"x"}`, nil, 409},
		{"revoke unknown key", "DELETE", "/admin/keys/" + uuid.NewString(), admin, "", nil, 404},
		{"revoke as viewer", "DELETE", "/admin/keys/" + keyID, viewer, "", nil, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, "http://localhost"+tc.path, strings.NewReader(tc.body))
			if tc.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			for i := 0; i+1 < len(tc.hdr); i += 2 {
				req.Header.Set(tc.hdr[i], tc.hdr[i+1])
			}
			route, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("the spec does not describe %s %s: %v", tc.method, tc.path, err)
			}

			rec := httptest.NewRecorder()
			r.admin.Handler().ServeHTTP(rec, req.Clone(context.Background()))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body)
			}
			body, _ := io.ReadAll(rec.Body)

			in := &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: route,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			}
			if tc.body != "" && tc.wantStatus != 400 {
				req.Body = io.NopCloser(strings.NewReader(tc.body))
				if err := openapi3filter.ValidateRequest(context.Background(), in); err != nil {
					t.Errorf("the request breaks the spec: %v", err)
				}
			}
			err = openapi3filter.ValidateResponse(context.Background(), &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in, Status: rec.Code, Header: rec.Header(), Body: io.NopCloser(strings.NewReader(string(body))),
			})
			if err != nil {
				t.Errorf("the response breaks the spec: %v\nbody: %s", err, body)
			}
		})
	}
}
