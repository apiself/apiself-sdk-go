package sdk

// Shared DB-backed catalogue store for AI boxes.
//
// Phase 4 refactor: every AI box (TTS, transcribe, llm, future image-gen,
// ...) keeps the union of preset + upstream + custom + installed model
// state in one SQLite table called `models`. The schema is identical
// across boxes; this helper provides the boilerplate so each box just
// wires its own *sql.DB handle and starts upserting.
//
// Source discriminator:
//   - "preset"   - shipped in config.json models.presets[], UPSERTed at
//                  box startup. URL / displayName authoritative from
//                  config.json on every restart.
//   - "upstream" - fetched live from an external catalogue (e.g. piper
//                  voices.json on HuggingFace). UPSERTed when the
//                  refresh job runs. Box-specific code owns the fetch.
//   - "custom"   - user-added through the box UI. Survives config.json
//                  changes. Only path that lets users plug in voices /
//                  models that aren't in the curated list.
//
// On-disk tracking:
//   - file_path  - absolute path to the primary file once downloaded.
//   - on_disk    - bool, true after a successful EnsureModel call. The
//                  UI gates Install / Use buttons off this.
//   - added_at   - when the row was inserted (preset = first startup,
//                  custom = user clicked Add, upstream = first refresh).
//   - last_used_at - bumped by box backends on synthesis / inference.
//                    Useful for "most recently used" UI sort + GC.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Model is the row shape every AI box's catalogue uses. Languages and
// CompanionURLs are stored as JSON arrays in their respective TEXT
// columns; the helpers below handle the marshal/unmarshal so callers
// see plain []string.
type Model struct {
	ID                string
	Family            string
	Source            string // "preset" | "upstream" | "custom"
	DisplayName       string
	Languages         []string
	URL               string
	CompanionURLs     []string
	SizeMB            int
	Quality           int // 1-5 stars
	License           string
	TierRequired      string // "free" | "basic" | "pro"
	DescriptionShort  string
	SpeedCPUxRealtime float64
	FilePath          string // "" until downloaded
	OnDisk            bool
	AddedAt           int64 // unix seconds
	LastUsedAt        int64 // unix seconds, 0 = never used
	// Added 2026-07: per-model file extension override + architecture kind
	// (sd/sdxl/flux/sd35) so a box can mix formats and pick the right engine
	// invocation; hardware hints for the picker's "fits my hardware" gate.
	Ext               string  // "" = use box-global fileExtension
	Kind              string  // "sd" | "sdxl" | "flux" | "sd35" | ""
	SpeedGPUxRealtime float64
	RAMRequiredMB     int
	VRAMRequiredMB    int
	GPURequired       bool
}

// ModelStore is the helper handle. Constructed once during box startup
// with the box's own *sql.DB; goroutine-safe via the underlying sql.DB.
type ModelStore struct {
	db     *sql.DB
	family string
}

// NewModelStore creates the table if missing and returns the helper.
// `family` is stored as a filter constant on every row so future boxes
// that span multiple families (e.g. one box serving both TTS + STT)
// can reuse the same table.
func NewModelStore(db *sql.DB, family string) (*ModelStore, error) {
	if db == nil || family == "" {
		return nil, fmt.Errorf("ModelStore: db and family required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS models (
			id                   TEXT NOT NULL,
			family               TEXT NOT NULL,
			source               TEXT NOT NULL,
			display_name         TEXT NOT NULL,
			languages_json       TEXT NOT NULL DEFAULT '[]',
			url                  TEXT NOT NULL DEFAULT '',
			companion_urls_json  TEXT NOT NULL DEFAULT '[]',
			size_mb              INTEGER NOT NULL DEFAULT 0,
			quality              INTEGER NOT NULL DEFAULT 3,
			license              TEXT NOT NULL DEFAULT '',
			tier_required        TEXT NOT NULL DEFAULT 'free',
			description_short    TEXT NOT NULL DEFAULT '',
			speed_cpu_x_realtime REAL NOT NULL DEFAULT 0,
			file_path            TEXT NOT NULL DEFAULT '',
			on_disk              INTEGER NOT NULL DEFAULT 0,
			added_at             INTEGER NOT NULL,
			last_used_at         INTEGER NOT NULL DEFAULT 0,
			ext                  TEXT NOT NULL DEFAULT '',
			kind                 TEXT NOT NULL DEFAULT '',
			speed_gpu_x_realtime REAL NOT NULL DEFAULT 0,
			ram_required_mb      INTEGER NOT NULL DEFAULT 0,
			vram_required_mb     INTEGER NOT NULL DEFAULT 0,
			gpu_required         INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (family, id)
		);
		CREATE INDEX IF NOT EXISTS idx_models_family ON models(family);
		CREATE INDEX IF NOT EXISTS idx_models_family_source ON models(family, source);
	`); err != nil {
		return nil, fmt.Errorf("ModelStore: migrate: %w", err)
	}
	// Additive migration for tables created before the 2026-07 columns landed.
	// SQLite has no "ADD COLUMN IF NOT EXISTS"; a duplicate-column error just
	// means the column is already there, so each ALTER is best-effort.
	for _, col := range []string{
		`ALTER TABLE models ADD COLUMN ext TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE models ADD COLUMN kind TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE models ADD COLUMN speed_gpu_x_realtime REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN ram_required_mb INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN vram_required_mb INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE models ADD COLUMN gpu_required INTEGER NOT NULL DEFAULT 0`,
	} {
		_, _ = db.Exec(col)
	}
	return &ModelStore{db: db, family: family}, nil
}

// Upsert inserts or replaces a row, preserving file_path / on_disk /
// last_used_at when those would otherwise be wiped by a config.json
// refresh of a preset row.
func (s *ModelStore) Upsert(m Model) error {
	if m.ID == "" {
		return fmt.Errorf("ModelStore.Upsert: id required")
	}
	if m.Family == "" {
		m.Family = s.family
	}
	if m.Source == "" {
		m.Source = "preset"
	}
	if m.AddedAt == 0 {
		m.AddedAt = time.Now().Unix()
	}
	langsJSON, _ := json.Marshal(m.Languages)
	companionsJSON, _ := json.Marshal(m.CompanionURLs)

	_, err := s.db.Exec(`
		INSERT INTO models (
			id, family, source, display_name, languages_json, url,
			companion_urls_json, size_mb, quality, license, tier_required,
			description_short, speed_cpu_x_realtime, file_path, on_disk,
			added_at, last_used_at,
			ext, kind, speed_gpu_x_realtime, ram_required_mb, vram_required_mb, gpu_required
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(family, id) DO UPDATE SET
			source               = excluded.source,
			display_name         = excluded.display_name,
			languages_json       = excluded.languages_json,
			url                  = excluded.url,
			companion_urls_json  = excluded.companion_urls_json,
			size_mb              = excluded.size_mb,
			quality              = excluded.quality,
			license              = excluded.license,
			tier_required        = excluded.tier_required,
			description_short    = excluded.description_short,
			speed_cpu_x_realtime = excluded.speed_cpu_x_realtime,
			ext                  = excluded.ext,
			kind                 = excluded.kind,
			speed_gpu_x_realtime = excluded.speed_gpu_x_realtime,
			ram_required_mb      = excluded.ram_required_mb,
			vram_required_mb     = excluded.vram_required_mb,
			gpu_required         = excluded.gpu_required
		`,
		m.ID, m.Family, m.Source, m.DisplayName, string(langsJSON), m.URL,
		string(companionsJSON), m.SizeMB, m.Quality, m.License, m.TierRequired,
		m.DescriptionShort, m.SpeedCPUxRealtime, m.FilePath, boolToInt(m.OnDisk),
		m.AddedAt, m.LastUsedAt,
		m.Ext, m.Kind, m.SpeedGPUxRealtime, m.RAMRequiredMB, m.VRAMRequiredMB, boolToInt(m.GPURequired),
	)
	return err
}

// List returns every row for this family in (source priority, displayName)
// order: presets first, upstream next, customs last so the UI's natural
// grouping ("official, then community, then yours") works without
// client-side sort. Within each source rows order by display_name.
func (s *ModelStore) List() ([]Model, error) {
	rows, err := s.db.Query(`
		SELECT id, family, source, display_name, languages_json, url,
		       companion_urls_json, size_mb, quality, license, tier_required,
		       description_short, speed_cpu_x_realtime, file_path, on_disk,
		       added_at, last_used_at,
		       ext, kind, speed_gpu_x_realtime, ram_required_mb, vram_required_mb, gpu_required
		  FROM models
		 WHERE family = ?
		 ORDER BY
		   CASE source
		     WHEN 'preset'   THEN 0
		     WHEN 'upstream' THEN 1
		     WHEN 'custom'   THEN 2
		     ELSE 3
		   END,
		   display_name ASC`,
		s.family)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one row or (nil, nil) when not found.
func (s *ModelStore) Get(id string) (*Model, error) {
	row := s.db.QueryRow(`
		SELECT id, family, source, display_name, languages_json, url,
		       companion_urls_json, size_mb, quality, license, tier_required,
		       description_short, speed_cpu_x_realtime, file_path, on_disk,
		       added_at, last_used_at,
		       ext, kind, speed_gpu_x_realtime, ram_required_mb, vram_required_mb, gpu_required
		  FROM models
		 WHERE family = ? AND id = ?
		 LIMIT 1`,
		s.family, id)
	m, err := scanModel(row)
	if err != nil {
		if strings.Contains(err.Error(), sql.ErrNoRows.Error()) {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

// Delete removes the row outright. Callers that want to keep history
// should set on_disk=false instead via MarkUninstalled.
func (s *ModelStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM models WHERE family = ? AND id = ?`, s.family, id)
	return err
}

// MarkInstalled flips on_disk=true + records the file path. Called by
// box backends after a successful EnsureModel(...) download.
func (s *ModelStore) MarkInstalled(id, filePath string) error {
	_, err := s.db.Exec(`
		UPDATE models
		   SET file_path = ?, on_disk = 1
		 WHERE family = ? AND id = ?`,
		filePath, s.family, id)
	return err
}

// MarkUninstalled flips on_disk=false and clears file_path. Used when
// the box backend detects the file has been removed (manual cleanup,
// disk full, ...). Doesn't remove the row.
func (s *ModelStore) MarkUninstalled(id string) error {
	_, err := s.db.Exec(`
		UPDATE models
		   SET file_path = '', on_disk = 0
		 WHERE family = ? AND id = ?`,
		s.family, id)
	return err
}

// TouchLastUsed updates last_used_at to now. Called by synth/inference
// handlers so the UI can show "most recently used" sorts.
func (s *ModelStore) TouchLastUsed(id string) error {
	_, err := s.db.Exec(`
		UPDATE models
		   SET last_used_at = ?
		 WHERE family = ? AND id = ?`,
		time.Now().Unix(), s.family, id)
	return err
}

// GarbageCollectPresets removes preset rows whose IDs are NOT in the
// keep set. Called after re-syncing presets from config.json: any
// preset removed from config.json upstream gets cleaned up, unless the
// user had already downloaded it (on_disk=true, kept for safety).
func (s *ModelStore) GarbageCollectPresets(keepIDs []string) error {
	if len(keepIDs) == 0 {
		// Defensive: if config.json has zero presets, don't wipe the
		// table - it's almost certainly a parsing bug, not a real
		// intention to remove every preset.
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(keepIDs)), ",")
	args := []any{s.family}
	for _, id := range keepIDs {
		args = append(args, id)
	}
	q := fmt.Sprintf(`
		DELETE FROM models
		 WHERE family = ?
		   AND source = 'preset'
		   AND on_disk = 0
		   AND id NOT IN (%s)`, placeholders)
	_, err := s.db.Exec(q, args...)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanModel(r rowScanner) (Model, error) {
	var m Model
	var langs, companions string
	var onDisk, gpuRequired int
	if err := r.Scan(
		&m.ID, &m.Family, &m.Source, &m.DisplayName, &langs, &m.URL,
		&companions, &m.SizeMB, &m.Quality, &m.License, &m.TierRequired,
		&m.DescriptionShort, &m.SpeedCPUxRealtime, &m.FilePath, &onDisk,
		&m.AddedAt, &m.LastUsedAt,
		&m.Ext, &m.Kind, &m.SpeedGPUxRealtime, &m.RAMRequiredMB, &m.VRAMRequiredMB, &gpuRequired,
	); err != nil {
		return Model{}, err
	}
	m.OnDisk = onDisk != 0
	m.GPURequired = gpuRequired != 0
	_ = json.Unmarshal([]byte(langs), &m.Languages)
	_ = json.Unmarshal([]byte(companions), &m.CompanionURLs)
	if m.Languages == nil {
		m.Languages = []string{}
	}
	if m.CompanionURLs == nil {
		m.CompanionURLs = []string{}
	}
	return m, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
