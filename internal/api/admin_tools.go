package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abdullah-9211/spillway/internal/auth"
	"github.com/abdullah-9211/spillway/internal/runs"
	"github.com/abdullah-9211/spillway/internal/tools"
)

// ToolsAdmin is the tool registry as the admin API uses it.
type ToolsAdmin interface {
	List(ctx context.Context) ([]tools.Tool, error)
	Create(ctx context.Context, p tools.CreateParams) (tools.Tool, error)
	Update(ctx context.Context, id uuid.UUID, p tools.UpdateParams) (tools.Tool, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// Discover fetches an MCP server's tool list, stores it, and returns the refreshed server.
	Discover(ctx context.Context, id uuid.UUID) (tools.Tool, error)
}

func (a *Admin) registerTools() {
	a.handle("GET", "/admin/tools", AccessViewer, a.toolsList)
	a.handle("POST", "/admin/tools", AccessAdmin, a.toolsCreate)
	a.handle("PUT", "/admin/tools/{id}", AccessAdmin, a.toolsUpdate)
	a.handle("DELETE", "/admin/tools/{id}", AccessAdmin, a.toolsDelete)
	a.handle("POST", "/admin/tools/{id}/discover", AccessAdmin, a.toolsDiscover)
}

type mcpToolJSON struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type toolJSON struct {
	ID               uuid.UUID       `json:"id"`
	Name             string          `json:"name"`
	Kind             tools.Kind      `json:"kind"`
	Endpoint         string          `json:"endpoint"`
	Description      string          `json:"description"`
	InputSchema      json.RawMessage `json:"input_schema,omitempty"`
	MCPTools         []mcpToolJSON   `json:"mcp_tools,omitempty"`
	HeaderNames      []string        `json:"header_names"` // the values are secrets and never leave the server
	TimeoutMS        int64           `json:"timeout_ms"`
	RequiresApproval bool            `json:"requires_approval"`
	DiscoveredAt     *time.Time      `json:"discovered_at"`
	CreatedAt        time.Time       `json:"created_at"`
}

func toolView(t tools.Tool) toolJSON {
	out := toolJSON{ID: t.ID, Name: t.Name, Kind: t.Kind, Endpoint: t.Endpoint, Description: t.Description, HeaderNames: []string{},
		TimeoutMS: t.Timeout.Milliseconds(), RequiresApproval: t.RequiresApproval, CreatedAt: t.CreatedAt}
	for k := range t.Headers { // names only; the values are secrets
		out.HeaderNames = append(out.HeaderNames, k)
	}
	slices.Sort(out.HeaderNames)
	if t.Kind == tools.MCP {
		list, _ := tools.DiscoveredTools(t.InputSchema)
		out.MCPTools = []mcpToolJSON{}
		for _, d := range list {
			out.MCPTools = append(out.MCPTools, mcpToolJSON{Name: t.Name + "." + d.Name, Description: d.Description, InputSchema: d.InputSchema})
		}
		if !t.Discovered.IsZero() {
			at := t.Discovered
			out.DiscoveredAt = &at
		}
	} else {
		out.InputSchema = t.InputSchema
	}
	return out
}

func (a *Admin) toolsList(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	list, err := a.d.Tools.List(r.Context())
	if err != nil {
		a.d.Log.Error("tools list", "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not load the tools.")
		return
	}
	out := make([]toolJSON, 0, len(list))
	for _, t := range list {
		out = append(out, toolView(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": out})
}

type toolBody struct {
	Name             string            `json:"name"`
	Kind             tools.Kind        `json:"kind"`
	Endpoint         string            `json:"endpoint"`
	Description      string            `json:"description"`
	Headers          map[string]string `json:"headers"`
	InputSchema      json.RawMessage   `json:"input_schema"`
	TimeoutMS        int64             `json:"timeout_ms"`
	RequiresApproval bool              `json:"requires_approval"`
}

func (a *Admin) toolError(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, tools.ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such tool.")
	case errors.Is(err, tools.ErrDuplicate):
		writeAdminError(w, http.StatusConflict, "tool_exists", "A tool with that name already exists.")
	case errors.Is(err, tools.ErrInvalid):
		writeAdminError(w, http.StatusBadRequest, "invalid_request", strings.TrimPrefix(err.Error(), tools.ErrInvalid.Error()+": "))
	default:
		a.d.Log.Error(what, "error", err)
		writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not "+what+". Try again.")
	}
}

func (a *Admin) toolsCreate(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	var b toolBody
	if !decodeStrict(w, r, &b) {
		return
	}
	if b.Kind == "" {
		b.Kind = tools.HTTP
	}
	t, err := a.d.Tools.Create(r.Context(), tools.CreateParams{Name: b.Name, Kind: b.Kind, Endpoint: b.Endpoint, Headers: b.Headers, Description: b.Description,
		InputSchema: b.InputSchema, Timeout: time.Duration(b.TimeoutMS) * time.Millisecond, RequiresApproval: b.RequiresApproval})
	if err != nil {
		a.toolError(w, err, "register the tool")
		return
	}
	writeJSON(w, http.StatusCreated, toolView(t))
}

func (a *Admin) toolsUpdate(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such tool.")
		return
	}
	var b struct {
		Endpoint         *string           `json:"endpoint"`
		Description      *string           `json:"description"`
		Headers          map[string]string `json:"headers"`
		InputSchema      json.RawMessage   `json:"input_schema"`
		TimeoutMS        *int64            `json:"timeout_ms"`
		RequiresApproval *bool             `json:"requires_approval"`
	}
	if !decodeStrict(w, r, &b) {
		return
	}
	p := tools.UpdateParams{Endpoint: b.Endpoint, Description: b.Description, Headers: b.Headers, InputSchema: b.InputSchema, RequiresApproval: b.RequiresApproval}
	if b.TimeoutMS != nil {
		d := time.Duration(*b.TimeoutMS) * time.Millisecond
		p.Timeout = &d
	}
	t, err := a.d.Tools.Update(r.Context(), id, p)
	if err != nil {
		a.toolError(w, err, "update the tool")
		return
	}
	writeJSON(w, http.StatusOK, toolView(t))
}

func (a *Admin) toolsDelete(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such tool.")
		return
	}
	if err := a.d.Tools.Delete(r.Context(), id); err != nil {
		a.toolError(w, err, "delete the tool")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) toolsDiscover(w http.ResponseWriter, r *http.Request, _ auth.Claims) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not_found", "There is no such tool.")
		return
	}
	t, err := a.d.Tools.Discover(r.Context(), id)
	var de *tools.DiscoverError
	switch {
	case errors.As(err, &de):
		writeAdminError(w, http.StatusBadGateway, "mcp_unreachable", de.Error())
		return
	case err != nil:
		a.toolError(w, err, "discover the server's tools")
		return
	}
	writeJSON(w, http.StatusOK, toolView(t))
}

// --- approvals ---

func (a *Admin) runsDecide(verb string) claimsHandler {
	return func(w http.ResponseWriter, r *http.Request, c auth.Claims) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
			return
		}
		var b struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 && !decodeStrict(w, r, &b) {
			return
		}
		b.Note = strings.TrimSpace(b.Note)
		if len(b.Note) > runs.MaxNoteBytes {
			writeAdminError(w, http.StatusBadRequest, "invalid_request", "The note is too long.")
			return
		}
		run, err := a.d.RunStarter.Decide(r.Context(), id, runs.Decision{Decision: verb, By: c.Username, Note: b.Note}, a.now())
		switch {
		case errors.Is(err, runs.ErrNotFound):
			writeAdminError(w, http.StatusNotFound, "not_found", "No run with that id.")
		case errors.Is(err, runs.ErrFinished):
			writeAdminError(w, http.StatusConflict, "run_finished", "The run has already finished.")
		case errors.Is(err, runs.ErrNotWaiting):
			writeAdminError(w, http.StatusConflict, "run_not_waiting", "The run is not waiting for a decision. Someone may have decided already.")
		case err != nil:
			a.d.Log.Error("decide run", "error", err)
			writeAdminError(w, http.StatusInternalServerError, "internal_error", "Could not record the decision. Try again.")
		default:
			writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID, "status": run.Status})
		}
	}
}
