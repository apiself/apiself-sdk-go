package sdk

// Agent panel HTTP endpointy. Mount jedným callom v box main.go:
//
//	store, _ := sdk.NewAgentStore(rawDB)
//	store.SeedFromFile(".apiself/agents.json")
//	sdk.RegisterAgentEndpoints(mux, store)
//
// Rovnaký pattern ako RegisterCrossBoxProxy - SDK vystavuje handlery
// uniformne, takže každý box dostane identickú sadu agent endpointov bez
// duplikácie. Spec: docs/box-agent-panel-spec.md.
//
// ⚠ Swagger sync (hard rule): tieto cesty MUSIA byť v .apiself/swagger.json
// každého boxu ktorý feature zapne. Boilerplate fragment je v
// docs/box-agent-panel-spec.md.
//
// Response envelope: { "success": bool, "data": ... } / { "success": false,
// "error": "..." } - zhodný s box konvenciou (types.Envelope), aby box UI
// nemuselo parsovať iný tvar.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// RegisterAgentEndpoints namountuje agent CRUD + runs + settings handlery.
func RegisterAgentEndpoints(mux *http.ServeMux, store *AgentStore) {
	h := &agentHandlers{store: store}
	// Collection: GET list / POST create
	mux.HandleFunc("/api/agents", h.agents)
	// Runs (musí byť pred /api/agents/ kvôli prefix matchingu v switchi)
	mux.HandleFunc("/api/agents/runs", h.runs)
	mux.HandleFunc("/api/agents/runs/", h.runByID)
	mux.HandleFunc("/api/agents/settings", h.settings)
	// Item: PUT update / DELETE  (/api/agents/{id})
	mux.HandleFunc("/api/agents/", h.agentByID)
}

type agentHandlers struct {
	store *AgentStore
}

func (h *agentHandlers) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (h *agentHandlers) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg})
}

// GET /api/agents  ·  POST /api/agents
func (h *agentHandlers) agents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := h.store.ListAgents()
		if err != nil {
			h.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.ok(w, map[string]any{"agents": list})
	case http.MethodPost:
		var body struct {
			Label  string `json:"label"`
			Prompt string `json:"prompt"`
			Icon   string `json:"icon"`
			Tier   string `json:"tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		a, err := h.store.CreateUserAgent(body.Label, body.Prompt, body.Icon, body.Tier)
		if err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, a)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// PUT /api/agents/{id}  ·  DELETE /api/agents/{id}
func (h *agentHandlers) agentByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if id == "" || strings.Contains(id, "/") {
		h.fail(w, http.StatusBadRequest, "agent id required")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Label  string `json:"label"`
			Prompt string `json:"prompt"`
			Icon   string `json:"icon"`
			Tier   string `json:"tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := h.store.UpdateUserAgent(id, body.Label, body.Prompt, body.Icon, body.Tier); err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, map[string]any{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteUserAgent(id); err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, map[string]any{"id": id})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// GET /api/agents/runs  ·  POST /api/agents/runs
func (h *agentHandlers) runs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		limit := 50
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil {
				limit = n
			}
		}
		list, err := h.store.ListRuns(limit)
		if err != nil {
			h.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.ok(w, map[string]any{"runs": list})
	case http.MethodPost:
		var run AgentRun
		if err := json.NewDecoder(r.Body).Decode(&run); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		id, err := h.store.SaveRun(run)
		if err != nil {
			h.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.ok(w, map[string]any{"id": id})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// GET /api/agents/runs/{id}
func (h *agentHandlers) runByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/runs/")
	if id == "" || strings.Contains(id, "/") {
		h.fail(w, http.StatusBadRequest, "run id required")
		return
	}
	run, err := h.store.GetRun(id)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if run == nil {
		h.fail(w, http.StatusNotFound, "run not found")
		return
	}
	h.ok(w, run)
}

// GET /api/agents/settings  ·  PUT /api/agents/settings
func (h *agentHandlers) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.ok(w, map[string]any{
			"maxIterations":     h.store.MaxIterations(),
			"crossBoxTools":     h.store.CrossBoxTools(),
			"systemPromptExtra": h.store.SystemPromptExtra(),
		})
	case http.MethodPut:
		var body struct {
			MaxIterations int `json:"maxIterations"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := h.store.SetMaxIterations(body.MaxIterations); err != nil {
			h.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.ok(w, map[string]any{"maxIterations": h.store.MaxIterations()})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
