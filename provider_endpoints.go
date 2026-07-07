package sdk

// RegisterProviderEndpoints mounts the cloud (BYOK) provider CRUD under a
// box-chosen prefix (e.g. "/api/llm/providers"). Part of the AI Studio
// standard (docs/box-ai-studio-spec.md). Response envelope matches the rest
// of the SDK: { success, data } / { success, error }.
//
//	GET    {prefix}                → { providers: [ {id,label,adapter,configured,models,docsUrl,source} ] }
//	POST   {prefix}                → set key {id, apiKey}  |  add custom {id, adapter, label, models?, docsUrl?, apiKey?}
//	DELETE {prefix}/{id}           → remove custom row, or clear a preset's key
//	GET    {prefix}/{id}/models    → live model list for the provider (via adapter, falls back to stored)

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// RegisterProviderEndpoints wires the provider handlers onto mux. auditNS is
// a short prefix for audit events (e.g. "llm"); pass "" to skip auditing.
func RegisterProviderEndpoints(mux *http.ServeMux, store *ProviderStore, prefix, auditNS string) {
	if store == nil || prefix == "" {
		return
	}
	h := &providerHandlers{store: store, prefix: strings.TrimRight(prefix, "/"), ns: auditNS}
	mux.HandleFunc(h.prefix, h.collection)
	mux.HandleFunc(h.prefix+"/", h.item)
}

type providerHandlers struct {
	store  *ProviderStore
	prefix string
	ns     string
}

func (h *providerHandlers) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (h *providerHandlers) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg})
}

func (h *providerHandlers) audit(verb, target string) {
	if h.ns != "" {
		Audit(h.ns+"."+verb, target, "")
	}
}

// GET {prefix} · POST {prefix}
func (h *providerHandlers) collection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := h.store.List()
		if err != nil {
			h.fail(w, http.StatusInternalServerError, "provider.list_failed")
			return
		}
		h.ok(w, map[string]any{"providers": list})
	case http.MethodPost:
		var body struct {
			ID      string   `json:"id"`
			Adapter string   `json:"adapter"`
			Label   string   `json:"label"`
			Models  []string `json:"models"`
			DocsURL string   `json:"docsUrl"`
			APIKey  string   `json:"apiKey"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.ID == "" {
			h.fail(w, http.StatusBadRequest, "request.body_invalid")
			return
		}
		// Unknown id + adapter given → add a custom provider (optionally with key).
		if _, exists := h.store.Get(body.ID); !exists {
			if body.Adapter == "" {
				h.fail(w, http.StatusBadRequest, "provider.adapter_required")
				return
			}
			p := Provider{ID: body.ID, Adapter: body.Adapter, Label: body.Label, Models: body.Models, DocsURL: body.DocsURL}
			if err := h.store.AddCustom(p, body.APIKey); err != nil {
				h.fail(w, http.StatusInternalServerError, "provider.save_failed")
				return
			}
			h.audit("provider_add", body.ID)
			h.ok(w, map[string]any{"ok": true})
			return
		}
		// Known provider → set/replace its key.
		if body.APIKey == "" {
			h.fail(w, http.StatusBadRequest, "provider.key_required")
			return
		}
		if err := h.store.SetKey(body.ID, body.APIKey); err != nil {
			h.fail(w, http.StatusInternalServerError, "provider.save_failed")
			return
		}
		h.audit("provider_set", body.ID)
		h.ok(w, map[string]any{"ok": true})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method.not_allowed")
	}
}

// {prefix}/{id}  ·  {prefix}/{id}/models
func (h *providerHandlers) item(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, h.prefix+"/")
	tail = strings.Trim(tail, "/")
	if tail == "" {
		h.fail(w, http.StatusNotFound, "provider.not_found")
		return
	}
	parts := strings.Split(tail, "/")
	id := parts[0]

	// GET {prefix}/{id}/models
	if len(parts) == 2 && parts[1] == "models" && r.Method == http.MethodGet {
		p, ok := h.store.Get(id)
		if !ok {
			h.fail(w, http.StatusNotFound, "provider.not_found")
			return
		}
		// Prefer a live adapter listing (uses the key when set); fall back
		// to the models stored on the row.
		if a, ok := CloudAdapterByID(p.Adapter); ok {
			key, _ := h.store.Key(id)
			if models, err := a.Models(r.Context(), key); err == nil {
				h.ok(w, map[string]any{"models": models})
				return
			}
		}
		out := make([]CloudModel, 0, len(p.Models))
		for _, m := range p.Models {
			out = append(out, CloudModel{ID: m})
		}
		h.ok(w, map[string]any{"models": out})
		return
	}

	// DELETE {prefix}/{id}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := h.store.Remove(id); err != nil {
			h.fail(w, http.StatusInternalServerError, "provider.delete_failed")
			return
		}
		h.audit("provider_remove", id)
		h.ok(w, map[string]any{"ok": true})
		return
	}

	h.fail(w, http.StatusMethodNotAllowed, "method.not_allowed")
}
