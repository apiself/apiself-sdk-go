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

	// Datasets - static asset bundles the box ships (preview MP3s for
	// the voice catalogue, language data, test corpora, ...). Each
	// entry is fetched + extracted once into {DataDir}/shared/datasets/<name>/
	// via sdk.EnsureDataset. See docs/box-ai-catalog-spec.md for the
	// full contract. Optional - leave nil for boxes that don't ship
	// any static assets.
	Datasets []BoxConfigDataset `json:"datasets,omitempty"`
}

// BoxConfigDataset declares a static asset bundle the box needs at
// runtime. See docs/box-ai-catalog-spec.md "Datasets" section.
type BoxConfigDataset struct {
	// Name is the stable identifier. Becomes the cache directory name
	// ({DataDir}/shared/datasets/<name>/). Pick a name that includes a
	// version suffix (e.g. "piper-samples-v1") so a future incompatible
	// bundle can roll out without invalidating the cache.
	Name string `json:"name"`

	// URL is the HTTPS URL to the archive. GitHub Release attachments
	// on the box's mono-repo are the recommended host.
	URL string `json:"url"`

	// SHA256 is the hex-encoded SHA-256 of the archive (NOT of the
	// extracted contents). Required - the SDK refuses to extract a
	// mismatched archive so a poisoned host cannot ship a different
	// payload than the one config.json pinned.
	SHA256 string `json:"sha256"`

	// SizeMB is an estimate used by progress UIs. Not enforced.
	SizeMB int `json:"sizeMb,omitempty"`

	// Archive is the archive format. Supported: "tar.gz" / "tar.bz2" /
	// "zip". Detected from URL extension; the field is informational.
	Archive string `json:"archive,omitempty"`

	// Description is a one-line note for human readers of config.json.
	Description string `json:"description,omitempty"`
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

// BoxConfigExternalDep is anything external the box needs on disk -
// a static binary, a portable runtime, a pip/npm dependency tree. Schema
// per feedback_box_external_deps_schema (memory).
//
// Required defaults to true when absent (`*bool` nil) so legacy configs
// (piper-style entries from 2023-24 without an explicit field) keep
// behaving as mandatory. New boxes SHOULD set it explicitly for clarity.
type BoxConfigExternalDep struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	OS      []string `json:"os,omitempty"`

	// Required gates the install flow. nil = "required (legacy default)";
	// explicit false = optional / on-demand feature (Pro tier, etc.).
	Required *bool `json:"required,omitempty"`

	// User-facing labels for the dependency. Both fields may be raw text
	// or "t:..."-prefixed locale refs; frontend resolves via useI18n().
	// Required entries leave these empty - the Manager renders them as
	// just the binary name.
	Feature   string `json:"feature,omitempty"`
	Rationale string `json:"rationale,omitempty"`

	// Total download envelope, used by Manager + box UI to render
	// "Will download ~1.5 GB" confirmations before starting work.
	SizeMB int `json:"size_mb,omitempty"`

	// TierRequired gates the install endpoint at the box side. Empty =
	// no tier gate. "basic" / "pro" matches sdk.HasTier(...) semantics.
	TierRequired string `json:"tier_required,omitempty"`

	// Provider decides who downloads this shared runtime. "box" (or "sdk")
	// means the box self-provisions on demand via sdk.EnsureSharedRuntime
	// (lazy, with progress) - the Manager SKIPS pre-fetch and env injection
	// for it. "manager" (or empty = legacy default) means the Manager
	// pre-fetches on install and injects APISELF_SHARED_<NAME>. Both write
	// the same {DataDir}/shared/{name}/{version}/ location so a runtime is
	// never downloaded twice regardless of who fetched it.
	Provider string `json:"provider,omitempty"`

	// On-demand control plane. For static-binary deps (piper, llama-cpp)
	// these stay empty - install happens implicitly on first use.
	// For runtime-style deps the Manager / box UI POSTs to TriggerEndpoint
	// and polls StatusEndpoint for progress.
	TriggerEndpoint string `json:"trigger_endpoint,omitempty"`
	StatusEndpoint  string `json:"status_endpoint,omitempty"`

	// Per-platform download map. Keys: "os-arch" tuples ("windows-amd64",
	// "darwin-arm64", ...). Empty for engines whose binary set is
	// computed differently (pure pip / npm deps without a base runtime).
	Downloads map[string]BoxConfigExternalDownload `json:"downloads,omitempty"`

	// Per-platform build-from-source recipes. Used when a platform has no
	// prebuilt asset (e.g. whisper.cpp on macOS). Same "os-arch" keys as
	// Downloads. For a given platform the installer prefers Downloads;
	// falls back to Build. Both install into the same shared/ location.
	Build map[string]BoxConfigExternalBuild `json:"build,omitempty"`

	// Python engines: pip-installed wheels with per-package progress.
	PipPackages   []BoxConfigPipPackage `json:"pip_packages,omitempty"`
	PipExtraIndex string                `json:"pip_extra_index,omitempty"`

	// Node engines: parallel shape to PipPackages.
	NpmPackages []BoxConfigNpmPackage `json:"npm_packages,omitempty"`
}

// BoxConfigExternalBuild is a generic build-from-source recipe for one
// platform. Recipe-driven so it can build anything (not runtime-specific):
// "cmake" runs a cmake configure + build; "shell" runs arbitrary commands
// (covers make, cargo, go build, ...). The build runs in a temp checkout;
// only Output (the primary binary) plus any Libs matches are installed into
// the shared cache dir, flat, so loader-relative (@rpath / $ORIGIN) sibling
// libs resolve.
type BoxConfigExternalBuild struct {
	// Recipe: "cmake" | "shell".
	Recipe string `json:"recipe"`

	// Source of the code to build (a shallow git checkout at Ref, or a
	// source tarball URL extracted first).
	Source BoxConfigBuildSource `json:"source"`

	// cmake recipe: flags passed at configure (`-D...`) + the build target.
	ConfigureFlags []string `json:"configure_flags,omitempty"`
	Target         string   `json:"target,omitempty"`

	// shell recipe: commands run sequentially in the checkout dir.
	Commands []string `json:"commands,omitempty"`

	// Extra environment for the build (e.g. MACOSX_DEPLOYMENT_TARGET, CC).
	Env map[string]string `json:"env,omitempty"`

	// Output is the primary built binary, relative to the checkout dir
	// (e.g. "build/bin/whisper-cli"). Binary is its installed name/rel-path
	// in the shared cache dir. Libs are glob patterns (relative to the
	// checkout dir) of sibling libraries copied next to the binary.
	Output string   `json:"output"`
	Binary string   `json:"binary"`
	Libs   []string `json:"libs,omitempty"`
}

// BoxConfigBuildSource points at the source tree for a build recipe.
type BoxConfigBuildSource struct {
	Git string `json:"git,omitempty"` // git repo URL (shallow clone at Ref)
	URL string `json:"url,omitempty"` // OR a source tarball URL
	Ref string `json:"ref,omitempty"` // tag / branch / commit
}

// IsRequired returns whether the dependency must succeed before the box
// can run. nil-pointer field defaults to true (legacy behaviour).
func (d *BoxConfigExternalDep) IsRequired() bool {
	if d == nil || d.Required == nil {
		return true
	}
	return *d.Required
}

// IsBoxProvided reports whether the box self-provisions this runtime via
// sdk.EnsureSharedRuntime (Provider "box"/"sdk"). When true the Manager
// skips pre-fetching + env injection for it. Empty/"manager" = legacy
// Manager-managed default.
func (d *BoxConfigExternalDep) IsBoxProvided() bool {
	if d == nil {
		return false
	}
	return d.Provider == "box" || d.Provider == "sdk"
}

// BoxConfigExternalDownload describes one platform-specific download
// artifact for a BoxConfigExternalDep. Archive format hints the install
// helper which extractor to use ("tar.gz-tree", "zip-dir", "deb", ...).
type BoxConfigExternalDownload struct {
	URL     string `json:"url"`
	Archive string `json:"archive,omitempty"`
	Inner   string `json:"inner,omitempty"`
	Binary  string `json:"binary,omitempty"`
	SizeMB  int    `json:"size_mb,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// BoxConfigPipPackage is one entry in a Python engine's install plan.
// Display + SizeMB drive the per-package progress UI while pip resolves
// the wheel tree.
type BoxConfigPipPackage struct {
	Spec    string `json:"spec"`              // pip requirement spec ("torch==2.4.0")
	Display string `json:"display,omitempty"` // human label ("PyTorch 2.4.0 (CPU)")
	SizeMB  int    `json:"size_mb,omitempty"` // rough wheel + transitive deps size
}

// BoxConfigNpmPackage mirrors BoxConfigPipPackage for Node engines.
type BoxConfigNpmPackage struct {
	Spec    string `json:"spec"`
	Display string `json:"display,omitempty"`
	SizeMB  int    `json:"size_mb,omitempty"`
}

// FindExternalDep returns the entry for the given dep name, or nil when
// the box doesn't declare it. Helper to keep box main.go terse.
func (c *BoxConfigFile) FindExternalDep(name string) *BoxConfigExternalDep {
	if c == nil {
		return nil
	}
	for i := range c.Dependencies.External {
		if c.Dependencies.External[i].Name == name {
			return &c.Dependencies.External[i]
		}
	}
	return nil
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
