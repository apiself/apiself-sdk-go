package sdk

// Unified box-dependency status for the SDK-UI <BoxDependencies> card.
//
// Aggregates everything a box needs to run its features - shared runtimes
// (ffmpeg, pandoc, chrome, whisper-cpp, ...), and (via registered providers)
// models - into one status snapshot so the user always sees what's installed,
// what isn't, and what each dep is for. Live install progress rides the "dep"
// / "runtime" / "model" SSE events; this endpoint is the snapshot the UI seeds
// from. See docs/box-dependencies-standard.md.
//
// Hard rule: this is READ-ONLY status. Installs happen via
// POST /api/runtime/install (RegisterRuntimeEndpoints) or a box's model
// install endpoint - never as a side effect of reading status, and never at
// box startup.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// DepStatus is one dependency (runtime / model / binary) + its install state.
type DepStatus struct {
	Kind         string `json:"kind"`         // "runtime" | "model" | "binary"
	Name         string `json:"name"`
	Version      string `json:"version,omitempty"`
	SizeMB       int    `json:"sizeMb,omitempty"`
	Purpose      string `json:"purpose,omitempty"`      // feature ref (t:...) - UI resolves
	TierRequired string `json:"tierRequired,omitempty"` // "", "basic", "pro"
	Required     bool   `json:"required"`
	Status       string `json:"status"`               // "not_installed" | "ready"
	InstalledAt  int64  `json:"installedAt,omitempty"` // unix sec (0 = not installed)
	Path         string `json:"path,omitempty"`        // when ready
}

// DepStatusProvider contributes extra deps (e.g. a box's models) to
// /api/deps/status. Register with RegisterDepStatusProvider.
type DepStatusProvider func() []DepStatus

var (
	depProviders   []DepStatusProvider
	depProvidersMu sync.Mutex
)

// RegisterDepStatusProvider adds a provider whose deps are merged into the
// /api/deps/status response (used by model-backed boxes to surface models
// alongside runtimes).
func RegisterDepStatusProvider(p DepStatusProvider) {
	if p == nil {
		return
	}
	depProvidersMu.Lock()
	depProviders = append(depProviders, p)
	depProvidersMu.Unlock()
}

// RegisterDepEndpoints mounts GET /api/deps/status - the aggregate snapshot of
// every shared runtime declared in config.json dependencies.external[] plus
// any provider-contributed deps (models). Read-only; never triggers a download.
func RegisterDepEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/api/deps/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		deps := runtimeDepStatuses()
		depProvidersMu.Lock()
		provs := append([]DepStatusProvider(nil), depProviders...)
		depProvidersMu.Unlock()
		for _, p := range provs {
			deps = append(deps, p()...)
		}
		if deps == nil {
			deps = []DepStatus{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"deps": deps},
		})
	})
}

// runtimeDepStatuses builds the runtime dep snapshot from config.json. Only
// deps that HAVE an install method on this platform (a download or build)
// appear - a Linux-only libgomp is omitted on Windows/macOS.
func runtimeDepStatuses() []DepStatus {
	cfg, err := LoadConfig()
	if err != nil {
		return nil
	}
	out := make([]DepStatus, 0, len(cfg.Dependencies.External))
	for i := range cfg.Dependencies.External {
		dep := &cfg.Dependencies.External[i]
		plan, err := resolveRuntimePlan(dep)
		if err != nil {
			continue // not installable on this platform
		}
		ds := DepStatus{
			Kind:         "runtime",
			Name:         dep.Name,
			Version:      dep.Version,
			SizeMB:       dep.SizeMB,
			Purpose:      dep.Feature,
			TierRequired: dep.TierRequired,
			Required:     dep.IsRequired(),
			Status:       "not_installed",
		}
		// Ready when the sentinel + binary are present (no download here).
		if _, err := os.Stat(filepath.Join(plan.cacheDir, ".ok")); err == nil {
			if fi, err := os.Stat(plan.cachedBin); err == nil && !fi.IsDir() {
				ds.Status = "ready"
				ds.Path = plan.cachedBin
				if ok, err := os.Stat(filepath.Join(plan.cacheDir, ".ok")); err == nil {
					ds.InstalledAt = ok.ModTime().Unix()
				}
			}
		}
		out = append(out, ds)
	}
	return out
}
