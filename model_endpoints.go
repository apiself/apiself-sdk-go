package sdk

// RegisterModelEndpoints mounts the box-owned local model catalogue under a
// prefix (e.g. "/api/image-gen/models"). The on-device half of the AI Studio
// standard (docs/box-ai-studio-spec.md). Generalises the previously
// hand-rolled per-box handlers (transcribe/tts/llm) into one SDK helper so a
// box wires its whole catalogue in a single call:
//
//	store, _ := sdk.NewModelStore(db, "image")
//	for _, m := range cfg.PresetsAsModels() { _ = store.Upsert(m) }
//	sdk.RegisterModelEndpoints(mux, store, sdk.ModelEndpointConfig{
//	    Prefix: "/api/image-gen/models", Family: "image",
//	    DisplayName: "Stable Diffusion", Engine: "stable-diffusion.cpp",
//	    FileExtension: "gguf", AuditNS: "imggen",
//	})
//
// Routes (consumed by the SDK-UI <AIModelPicker> / <BoxAIStudio>):
//
//	GET    {prefix}/catalog          → { family, displayName, engine, fileExtension, models:[...] }
//	POST   {prefix}/install          {model_id}  → downloads via EnsureModel, marks on-disk
//	POST   {prefix}/custom           {displayName, url, sizeMb?, languages?, tier?}
//	DELETE {prefix}/custom/{id}

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelEndpointConfig parametrises the catalogue for one box/family.
type ModelEndpointConfig struct {
	Prefix         string        // "/api/image-gen/models"
	Family         string        // "image" - EnsureModel + ModelStore namespace
	DisplayName    string        // "Stable Diffusion (image)"
	Engine         string        // "stable-diffusion.cpp"
	FileExtension  string        // "gguf" | "bin" | ...
	InstallTimeout time.Duration // default 30m
	AuditNS        string        // "imggen" - "" skips auditing
}

// RegisterModelEndpoints wires the catalogue handlers onto mux.
func RegisterModelEndpoints(mux *http.ServeMux, store *ModelStore, cfg ModelEndpointConfig) {
	if store == nil || cfg.Prefix == "" {
		return
	}
	if cfg.InstallTimeout == 0 {
		cfg.InstallTimeout = 30 * time.Minute
	}
	prefix := strings.TrimRight(cfg.Prefix, "/")
	h := &modelHandlers{store: store, cfg: cfg, prefix: prefix}
	mux.HandleFunc(prefix+"/catalog", h.catalog)
	mux.HandleFunc(prefix+"/install", h.install)
	mux.HandleFunc(prefix+"/custom", h.customAdd)
	mux.HandleFunc(prefix+"/custom/", h.customDelete)
}

type modelHandlers struct {
	store  *ModelStore
	cfg    ModelEndpointConfig
	prefix string
}

func (h *modelHandlers) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}

func (h *modelHandlers) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg})
}

func (h *modelHandlers) audit(verb, target, detail string) {
	if h.cfg.AuditNS != "" {
		Audit(h.cfg.AuditNS+"."+verb, target, detail)
	}
}

func (h *modelHandlers) catalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "http.get_only")
		return
	}
	rows, err := h.store.List()
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "model.list_failed")
		return
	}
	models := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		models = append(models, map[string]any{
			"id":                m.ID,
			"family":            m.Family,
			"source":            m.Source,
			"displayName":       m.DisplayName,
			"sizeMb":            m.SizeMB,
			"languages":         m.Languages,
			"quality":           m.Quality,
			"speedCpuXRealtime": m.SpeedCPUxRealtime,
			"descriptionShort":  m.DescriptionShort,
			"url":               m.URL,
			"companionUrls":     m.CompanionURLs,
			"license":           m.License,
			"tierRequired":      m.TierRequired,
			"onDisk":            m.OnDisk,
			"ext":               m.Ext,
			"kind":              m.Kind,
			"speedGpuXRealtime": m.SpeedGPUxRealtime,
			"ramRequiredMb":     m.RAMRequiredMB,
			"vramRequiredMb":    m.VRAMRequiredMB,
			"gpuRequired":       m.GPURequired,
		})
	}
	h.ok(w, map[string]any{
		"family":        h.cfg.Family,
		"displayName":   h.cfg.DisplayName,
		"engine":        h.cfg.Engine,
		"fileExtension": h.cfg.FileExtension,
		"models":        models,
	})
}

func (h *modelHandlers) install(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, "http.post_only")
		return
	}
	var body struct {
		ModelID string `json:"model_id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		h.fail(w, http.StatusBadRequest, "request.body_invalid")
		return
	}
	body.ModelID = strings.TrimSpace(body.ModelID)
	if body.ModelID == "" {
		h.fail(w, http.StatusBadRequest, "request.body_invalid")
		return
	}
	m, err := h.store.Get(body.ModelID)
	if err != nil || m == nil {
		h.fail(w, http.StatusNotFound, "model.not_found")
		return
	}
	// Per-model extension override lets one family mix e.g. .gguf and
	// .safetensors; fall back to the box-global extension when unset.
	ext := m.Ext
	if ext == "" {
		ext = h.cfg.FileExtension
	}
	path, err := EnsureModel(h.cfg.Family, m.ID, ext, m.URL, m.CompanionURLs, h.cfg.InstallTimeout)
	if err != nil {
		Log.Warn("models.install_failed", "id", m.ID, "err", err.Error())
		h.fail(w, http.StatusInternalServerError, "model.install_failed")
		return
	}
	if err := h.store.MarkInstalled(m.ID, path); err != nil {
		h.fail(w, http.StatusInternalServerError, "model.mark_failed")
		return
	}
	h.audit("model_install", m.ID, m.DisplayName)
	h.ok(w, map[string]any{"model_id": m.ID, "file_path": path, "installed": true})
}

func (h *modelHandlers) customAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.fail(w, http.StatusMethodNotAllowed, "http.post_only")
		return
	}
	var body struct {
		DisplayName string   `json:"displayName"`
		URL         string   `json:"url"`
		SizeMB      int      `json:"sizeMb"`
		Languages   []string `json:"languages"`
		Quality     int      `json:"quality"`
		Tier        string   `json:"tier"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		h.fail(w, http.StatusBadRequest, "request.body_invalid")
		return
	}
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	body.URL = strings.TrimSpace(body.URL)
	if body.DisplayName == "" || body.URL == "" ||
		(!strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "http://")) {
		h.fail(w, http.StatusBadRequest, "request.body_invalid")
		return
	}
	tier := body.Tier
	if tier == "" {
		tier = "free"
	}
	id := modelSlug(body.DisplayName) + "-" + randomModelSlug(6)
	if err := h.store.Upsert(Model{
		ID:               id,
		Family:           h.cfg.Family,
		Source:           "custom",
		DisplayName:      body.DisplayName,
		URL:              body.URL,
		SizeMB:           body.SizeMB,
		Languages:        body.Languages,
		Quality:          body.Quality,
		License:          "user-supplied",
		TierRequired:     tier,
		DescriptionShort: "Custom model (user-added)",
	}); err != nil {
		h.fail(w, http.StatusInternalServerError, "model.save_failed")
		return
	}
	h.audit("model_custom_add", id, body.DisplayName)
	h.ok(w, map[string]any{"id": id, "source": "custom"})
}

func (h *modelHandlers) customDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.fail(w, http.StatusMethodNotAllowed, "http.delete_only")
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, h.prefix+"/custom/"), "/")
	if id == "" {
		h.fail(w, http.StatusBadRequest, "request.body_invalid")
		return
	}
	m, err := h.store.Get(id)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "model.get_failed")
		return
	}
	if m == nil {
		h.fail(w, http.StatusNotFound, "model.not_found")
		return
	}
	if m.Source != "custom" {
		h.fail(w, http.StatusBadRequest, "model.not_custom")
		return
	}
	if m.FilePath != "" {
		_ = os.Remove(m.FilePath)
		for _, cu := range m.CompanionURLs {
			_ = os.Remove(filepath.Join(filepath.Dir(m.FilePath), filepath.Base(cu)))
		}
	}
	if err := h.store.Delete(id); err != nil {
		h.fail(w, http.StatusInternalServerError, "model.delete_failed")
		return
	}
	h.audit("model_custom_delete", id, m.DisplayName)
	h.ok(w, map[string]any{"id": id, "deleted": true})
}

// ── small helpers ─────────────────────────────────────────────────────────

func modelSlug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomModelSlug(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		ts := time.Now().UnixNano()
		for i := range buf {
			buf[i] = alphabet[ts%int64(len(alphabet))]
			ts /= int64(len(alphabet))
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}
