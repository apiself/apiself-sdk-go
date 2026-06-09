package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// BoxConfigFile reprezentuje obsah `.apiself/config.json` (canonical source-of-truth
// pre box metadata: ID, name, version atd.).
//
// Pouzitie:
//
//	cfg, _ := sdk.LoadConfig()
//	license := sdk.InitBox(sdk.BoxConfig{
//	    ID:      cfg.ID,
//	    Name:    cfg.Name,
//	    Version: cfg.Version,
//	})
//
// Nahradzuje hardcoded `Version: "1.0.0"` v main.go ktore sa pri kazdom bumpe
// musi rucne menit. Po zmene config.json sa pri dalsom buil-de automaticky
// reflektne v `/api/info` aj v Manager UI.
type BoxConfigFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Port        int    `json:"port,omitempty"`

	// BrandColor is an explicit hex like "#14b8a6". When empty, callers fall
	// back to scraping the first <rect fill="…"> from ui/public/icon.svg
	// (legacy behaviour preserved by the SDK UI BoxLayout). Explicit value
	// avoids that regex match and lets stroke-only glyphs still tint the
	// sidebar consistently.
	BrandColor string `json:"brand_color,omitempty"`

	// Category groups the box in the marketplace catalogue. Free-form
	// today (see admin/box_categories table for the recommended enum:
	// audio, video, storage, communications, ai, developer, productivity,
	// utility, ...).
	Category string `json:"category,omitempty"`

	// Tagline is a one-line marketing hook (~60 chars max). Lokalizable
	// via t: prefix: "t:config.tagline" -> looked up in .apiself/locales/*.json.
	// Used by the marketplace card + Manager Box Hub tile.
	Tagline string `json:"tagline,omitempty"`

	// Dependencies - soft cross-box deps declared by the box author.
	// Lifted off .apiself/config.json's `dependencies.boxes[]` so callers
	// don't have to re-parse the file. SDK UI BoxDependencies component
	// reads BoxInfo.Dependencies and auto-renders one card per entry.
	Dependencies BoxConfigDependencies `json:"dependencies,omitempty"`

	// Models - AI model catalogue declared by the box. Boxes that ship a
	// model picker (TTS voices, STT models, LLM weights, ...) put their
	// preset list here. The SDK ModelStore lifecycle sync at startup
	// UPSERTs each preset into the box DB so the catalogue UI reads from
	// one source of truth (the `models` table) instead of having to
	// re-parse config.json on every request. Optional - leave nil for
	// boxes without any AI-model story.
	Models *BoxConfigModels `json:"models,omitempty"`
}

// BoxConfigModels declares an AI-model catalogue owned by the box.
//
// Layout:
//
//   - Family / Engine / FileExtension : describe the family the box
//     downloads into the shared cache. Family is the cache subdirectory
//     under {DataDir}/shared/ai-models/ - boxes sharing a family
//     (multiple TTS boxes both consuming piper voices) reuse the same
//     on-disk files via sdk.EnsureModel.
//   - UpstreamURL / UpstreamBaseDL : optional - when the box has a live
//     external catalogue (rhasspy/piper-voices on HuggingFace, the LLM
//     world's various hub.tx-like indexes), the box-side sync code
//     fetches UpstreamURL, parses upstream rows, and UPSERTs them into
//     the models table with source="upstream". UpstreamBaseDL is the
//     prefix for relative file paths in the upstream feed - typically
//     "https://huggingface.co/<repo>/resolve/main" so a feed entry
//     "en/en_US/lessac/medium/en_US-lessac-medium.onnx" resolves to a
//     full HTTPS URL by simple concatenation.
//   - Presets : the curated baseline shipped in the box itself. Always
//     loaded, no network. The UI shows these first so first-run + offline
//     users can still install something.
type BoxConfigModels struct {
	Family         string                 `json:"family"`
	Engine         string                 `json:"engine,omitempty"`
	FileExtension  string                 `json:"fileExtension"`
	UpstreamURL    string                 `json:"upstream_url,omitempty"`
	UpstreamBaseDL string                 `json:"upstream_base_dl,omitempty"`
	Presets        []BoxConfigModelPreset `json:"presets,omitempty"`
}

// BoxConfigModelPreset is one curated model entry. The fields mirror
// sdk.Model (the DB row shape) so converting one to the other is a flat
// copy via (c *BoxConfigFile).PresetsAsModels().
type BoxConfigModelPreset struct {
	ID                string   `json:"id"`
	DisplayName       string   `json:"displayName"`
	URL               string   `json:"url"`
	CompanionURLs     []string `json:"companionUrls,omitempty"`
	SizeMB            int      `json:"sizeMb,omitempty"`
	Languages         []string `json:"languages,omitempty"`
	Quality           int      `json:"quality,omitempty"`
	SpeedCPUxRealtime float64  `json:"speedCpuXRealtime,omitempty"`
	DescriptionShort  string   `json:"descriptionShort,omitempty"`
	License           string   `json:"license,omitempty"`
	TierRequired      string   `json:"tierRequired,omitempty"`
}

// PresetsAsModels converts the config.json preset list into the
// ModelStore row shape so a box can do:
//
//	for _, m := range cfg.PresetsAsModels() { _ = store.Upsert(m) }
//
// Family defaults to cfg.Models.Family. TierRequired defaults to "free".
// Returns an empty slice when cfg.Models is nil.
func (c *BoxConfigFile) PresetsAsModels() []Model {
	if c == nil || c.Models == nil {
		return nil
	}
	family := c.Models.Family
	out := make([]Model, 0, len(c.Models.Presets))
	for _, p := range c.Models.Presets {
		tier := p.TierRequired
		if tier == "" {
			tier = "free"
		}
		out = append(out, Model{
			ID:                p.ID,
			Family:            family,
			Source:            "preset",
			DisplayName:       p.DisplayName,
			Languages:         p.Languages,
			URL:               p.URL,
			CompanionURLs:     p.CompanionURLs,
			SizeMB:            p.SizeMB,
			Quality:           p.Quality,
			License:           p.License,
			TierRequired:      tier,
			DescriptionShort:  p.DescriptionShort,
			SpeedCPUxRealtime: p.SpeedCPUxRealtime,
		})
	}
	return out
}

// BoxConfigDependencies mirrors the `dependencies` object in config.json.
// `external` is OS-level (binaries the box wants on PATH); `boxes` is the
// soft cross-box list used by Dashboard / Box Hub tiles.
type BoxConfigDependencies struct {
	External []BoxConfigExternalDep `json:"external,omitempty"`
	Boxes    []BoxConfigBoxDep      `json:"boxes,omitempty"`
}

// BoxConfigExternalDep is an OS-level binary the box wants on PATH.
type BoxConfigExternalDep struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	OS      []string `json:"os,omitempty"`
}

// BoxConfigBoxDep is a soft cross-box dependency. `feature` and
// `rationale` may be raw text or "t:..."-prefixed locale refs - the SDK
// keeps them as-is, frontend resolves via useI18n().t() when displaying.
type BoxConfigBoxDep struct {
	BoxID     string `json:"box_id"`
	Required  bool   `json:"required,omitempty"`
	Since     string `json:"since,omitempty"`
	Feature   string `json:"feature,omitempty"`
	Rationale string `json:"rationale,omitempty"`
}

// LoadConfig nacita `.apiself/config.json` zo standardnych cestiek (relativnych
// k binarke aj k cwd) a vrati parsed BoxConfigFile.
//
// Skusane cesty (v poradi):
//  1. ./.apiself/config.json (cwd)
//  2. ../.apiself/config.json (binarka v bin/)
//  3. {exe-dir}/.apiself/config.json
//  4. {exe-dir}/../.apiself/config.json
//
// Ak ziadny path nezbeha, vrati chybu. Box main.go by mal pouzit `Must` pattern:
//
//	cfg, err := sdk.LoadConfig()
//	if err != nil {
//	    log.Fatalf("config.json not found: %v", err)
//	}
func LoadConfig() (*BoxConfigFile, error) {
	candidates := []string{
		filepath.Join(".apiself", "config.json"),
		filepath.Join("..", ".apiself", "config.json"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, ".apiself", "config.json"),
			filepath.Join(dir, "..", ".apiself", "config.json"),
		)
	}

	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg BoxConfigFile
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if cfg.ID == "" {
			continue
		}
		return &cfg, nil
	}
	return nil, errors.New("sdk.LoadConfig: .apiself/config.json not found in any candidate path")
}

// MustLoadConfig je convenience wrapper - panic-uje ak config nie je dostupny.
// Pouzivaj v main.go pri startup-e ked je config nutnost.
func MustLoadConfig() *BoxConfigFile {
	cfg, err := LoadConfig()
	if err != nil {
		panic(err)
	}
	return cfg
}
