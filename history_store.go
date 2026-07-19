package sdk

// Shared generation-history store for AI media boxes (image-gen, video-gen).
//
// Every render persists two things:
//   1. the output file  -> {outputs root}/YYYY-MM/gen_<ts>_<rand>.<ext>
//                          plus a small JPEG thumbnail under thumbs/
//   2. a gen_history row -> full reproduction metadata (prompt, seed, model,
//                          params, exec time, relative file paths)
//
// The DB stores paths RELATIVE to the outputs root so the user can move the
// root (box Settings) without breaking history. Bytes never go into SQLite.
//
// Retention: optional (default unlimited). Age/size limits delete files of
// non-favorite rows only; the row itself is kept with file_deleted=1 so
// history never loses the "what/how" record, only the bytes.
//
// Kind discriminator ("image" | "video") lives on every row; kind-specific
// params (video: duration/ratio/resolution/fps) go into extra_json.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decode support for thumbnails
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GenRecord is one row of generation history.
type GenRecord struct {
	ID          int64          `json:"id"`
	Kind        string         `json:"kind"`
	JobID       string         `json:"jobId,omitempty"`
	Prompt      string         `json:"prompt"`
	Negative    string         `json:"negative,omitempty"`
	Preset      string         `json:"preset,omitempty"`
	Seed        int64          `json:"seed"`
	Model       string         `json:"model"`
	Engine      string         `json:"engine"`
	Steps       int            `json:"steps,omitempty"`
	CfgScale    float64        `json:"cfgScale,omitempty"`
	Sampler     string         `json:"sampler,omitempty"`
	Scheduler   string         `json:"scheduler,omitempty"`
	Width       int            `json:"width,omitempty"`
	Height      int            `json:"height,omitempty"`
	ExecMS      int64          `json:"execMs"`
	FilePath    string         `json:"filePath"`  // relative to outputs root
	ThumbPath   string         `json:"thumbPath"` // relative to outputs root
	StorageRef  string         `json:"storageRef,omitempty"`
	OK          bool           `json:"ok"`
	Error       string         `json:"error,omitempty"`
	Favorite    bool           `json:"favorite"`
	FileDeleted bool           `json:"fileDeleted"`
	Extra       map[string]any `json:"extra,omitempty"`
	CreatedAt   int64          `json:"createdAt"`
}

// HistoryRetention is the user-configurable cleanup policy. Zero values mean
// "no limit". Favorites are never touched.
type HistoryRetention struct {
	MaxAgeDays int `json:"maxAgeDays"`
	MaxTotalMB int `json:"maxTotalMB"`
}

// StorageBoxID - the storage box (cross-box /api/cb/save target).
const StorageBoxID = "apiself-box-storage"

// HistoryConfig is the user-configurable output layout + storage push policy.
// Zero values fall back to defaults (BoxDataDir outputs, month subdirs,
// "gen_{ts}_{rand}" names, no auto-push).
type HistoryConfig struct {
	// OutputsRoot - absolute directory for rendered files. "" = default
	// {BoxDataDir}/outputs.
	OutputsRoot string `json:"outputsRoot"`
	// SubdirPattern - how renders are grouped: "month" (YYYY-MM, default),
	// "day" (YYYY-MM-DD) or "flat" (no subdirectory).
	SubdirPattern string `json:"subdirPattern"`
	// FilePattern - file name template without extension. Tokens:
	// {yyyy} {mm} {dd} {hh} {mi} {ss} {ts} {rand}. Default "gen_{ts}_{rand}".
	FilePattern string `json:"filePattern"`
	// StorageAutoPush - when true every successful render is also pushed to
	// the storage box (cross-box /api/cb/save) using StorageProfileID.
	StorageAutoPush   bool   `json:"storageAutoPush"`
	StorageProfileID  string `json:"storageProfileId"`
	// StoragePathPrefix - destination folder inside the storage profile
	// (e.g. "image-gen"). The render's file name is appended.
	StoragePathPrefix string `json:"storagePathPrefix"`
}

// HistoryStore is the helper handle. Goroutine-safe via the underlying
// sql.DB; file writes are best-effort serialized by the OS.
type HistoryStore struct {
	db    *sql.DB
	boxID string
	kind  string
	root  string // outputs root (absolute)
}

// NewHistoryStore creates the gen_history + history_settings tables and
// resolves the outputs root ({BoxDataDir}/outputs by default, overridable
// via SetOutputsRoot / the history_settings row).
func NewHistoryStore(db *sql.DB, boxID, kind string) (*HistoryStore, error) {
	if db == nil || boxID == "" || kind == "" {
		return nil, fmt.Errorf("HistoryStore: db, boxID and kind required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS gen_history (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			kind         TEXT NOT NULL,
			job_id       TEXT NOT NULL DEFAULT '',
			prompt       TEXT NOT NULL DEFAULT '',
			negative     TEXT NOT NULL DEFAULT '',
			preset       TEXT NOT NULL DEFAULT '',
			seed         INTEGER NOT NULL DEFAULT 0,
			model        TEXT NOT NULL DEFAULT '',
			engine       TEXT NOT NULL DEFAULT '',
			steps        INTEGER NOT NULL DEFAULT 0,
			cfg_scale    REAL NOT NULL DEFAULT 0,
			sampler      TEXT NOT NULL DEFAULT '',
			scheduler    TEXT NOT NULL DEFAULT '',
			width        INTEGER NOT NULL DEFAULT 0,
			height       INTEGER NOT NULL DEFAULT 0,
			exec_ms      INTEGER NOT NULL DEFAULT 0,
			file_path    TEXT NOT NULL DEFAULT '',
			thumb_path   TEXT NOT NULL DEFAULT '',
			storage_ref  TEXT NOT NULL DEFAULT '',
			ok           INTEGER NOT NULL DEFAULT 1,
			error        TEXT NOT NULL DEFAULT '',
			favorite     INTEGER NOT NULL DEFAULT 0,
			file_deleted INTEGER NOT NULL DEFAULT 0,
			extra_json   TEXT NOT NULL DEFAULT '{}',
			created_at   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_gen_history_created ON gen_history(created_at);
		CREATE INDEX IF NOT EXISTS idx_gen_history_favorite ON gen_history(favorite);
		CREATE TABLE IF NOT EXISTS history_settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return nil, fmt.Errorf("HistoryStore: create tables: %w", err)
	}
	s := &HistoryStore{db: db, boxID: boxID, kind: kind}
	if v := s.setting("outputs_root"); v != "" {
		s.root = v
	} else {
		s.root = filepath.Join(BoxDataDir(boxID), "outputs")
	}
	return s, nil
}

// OutputsRoot returns the current absolute outputs root.
func (s *HistoryStore) OutputsRoot() string { return s.root }

// SetOutputsRoot persists a user-chosen outputs directory (box Settings).
// Existing files are NOT moved; old rows keep resolving against the new
// root only if the user moved the files themselves. Empty resets to default.
func (s *HistoryStore) SetOutputsRoot(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		s.root = filepath.Join(BoxDataDir(s.boxID), "outputs")
		return s.setSetting("outputs_root", "")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("HistoryStore: outputs root: %w", err)
	}
	s.root = dir
	return s.setSetting("outputs_root", dir)
}

func (s *HistoryStore) setting(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM history_settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *HistoryStore) setSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO history_settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Config returns the stored output/storage policy (defaults filled in).
func (s *HistoryStore) Config() HistoryConfig {
	var c HistoryConfig
	if v := s.setting("config"); v != "" {
		_ = json.Unmarshal([]byte(v), &c)
	}
	c.OutputsRoot = s.setting("outputs_root") // canonical key (SetOutputsRoot)
	if c.SubdirPattern == "" {
		c.SubdirPattern = "month"
	}
	if c.FilePattern == "" {
		c.FilePattern = "gen_{ts}_{rand}"
	}
	return c
}

// SetConfig persists the policy. OutputsRoot goes through SetOutputsRoot so
// the directory is created/validated and the live root switches immediately.
func (s *HistoryStore) SetConfig(c HistoryConfig) error {
	if err := s.SetOutputsRoot(c.OutputsRoot); err != nil {
		return err
	}
	c.OutputsRoot = "" // stored separately under outputs_root
	b, _ := json.Marshal(c)
	return s.setSetting("config", string(b))
}

// renderName expands the FilePattern tokens for "now". The result is
// sanitized to a safe file name (no path separators).
func renderName(pattern string, now time.Time) string {
	r := strings.NewReplacer(
		"{yyyy}", now.Format("2006"),
		"{mm}", now.Format("01"),
		"{dd}", now.Format("02"),
		"{hh}", now.Format("15"),
		"{mi}", now.Format("04"),
		"{ss}", now.Format("05"),
		"{ts}", fmt.Sprintf("%d", now.Unix()),
		"{rand}", fmt.Sprintf("%04d", rand.Intn(10000)),
	)
	name := r.Replace(pattern)
	name = strings.Map(func(c rune) rune {
		switch c {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return c
	}, name)
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("gen_%d_%04d", now.Unix(), rand.Intn(10000))
	}
	return name
}

// subdirFor maps the SubdirPattern to the dated folder for "now".
func subdirFor(pattern string, now time.Time) string {
	switch pattern {
	case "flat":
		return ""
	case "day":
		return now.Format("2006-01-02")
	default: // "month"
		return now.Format("2006-01")
	}
}

// SaveOutput writes the rendered bytes under the outputs root and returns
// paths RELATIVE to it. thumb may be nil: for common image extensions a
// thumbnail is generated automatically; for anything else (video) the
// caller may pass poster bytes (JPEG) or nil for no thumbnail.
func (s *HistoryStore) SaveOutput(data []byte, ext string, thumb []byte) (relFile, relThumb string, err error) {
	ext = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(ext)), ".")
	if ext == "" {
		return "", "", fmt.Errorf("HistoryStore.SaveOutput: ext required")
	}
	cfg := s.Config()
	now := time.Now()
	sub := subdirFor(cfg.SubdirPattern, now)
	name := renderName(cfg.FilePattern, now)
	dir := filepath.Join(s.root, sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("HistoryStore.SaveOutput: mkdir: %w", err)
	}
	// Uniqueness: a pattern without {ts}/{rand} (e.g. "{yyyy}{mm}{dd}") would
	// collide on the second render of the day - suffix until free.
	base := name
	for i := 0; ; i++ {
		if _, err := os.Stat(filepath.Join(dir, name+"."+ext)); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s_%03d", base, i+1)
	}
	relFile = filepath.ToSlash(filepath.Join(sub, name+"."+ext))
	if err := os.WriteFile(filepath.Join(s.root, filepath.FromSlash(relFile)), data, 0o644); err != nil {
		return "", "", fmt.Errorf("HistoryStore.SaveOutput: write: %w", err)
	}

	if thumb == nil {
		switch ext {
		case "png", "jpg", "jpeg", "webp":
			thumb = makeJPEGThumb(data, 256)
		}
	}
	if thumb != nil {
		tdir := filepath.Join(s.root, "thumbs")
		if err := os.MkdirAll(tdir, 0o755); err == nil {
			relThumb = filepath.ToSlash(filepath.Join("thumbs", name+".jpg"))
			if werr := os.WriteFile(filepath.Join(s.root, filepath.FromSlash(relThumb)), thumb, 0o644); werr != nil {
				relThumb = "" // thumbnail is best-effort
			}
		}
	}
	return relFile, relThumb, nil
}

// makeJPEGThumb decodes an image and box-downscales its longer side to
// maxDim, returning JPEG bytes. Returns nil on any failure (best-effort).
func makeJPEGThumb(data []byte, maxDim int) []byte {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}
	scale := 1.0
	if w > h {
		scale = float64(maxDim) / float64(w)
	} else {
		scale = float64(maxDim) / float64(h)
	}
	if scale > 1 {
		scale = 1
	}
	tw, th := int(float64(w)*scale), int(float64(h)*scale)
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	// Box-average sampling: each destination pixel averages its source rect.
	for y := 0; y < th; y++ {
		sy0, sy1 := y*h/th, (y+1)*h/th
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < tw; x++ {
			sx0, sx1 := x*w/tw, (x+1)*w/tw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, bl, _ := src.At(b.Min.X+sx, b.Min.Y+sy).RGBA()
					rs += uint64(r >> 8)
					gs += uint64(g >> 8)
					bs += uint64(bl >> 8)
					n++
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(rs / n)
			dst.Pix[i+1] = uint8(gs / n)
			dst.Pix[i+2] = uint8(bs / n)
			dst.Pix[i+3] = 255
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil
	}
	return out.Bytes()
}

// Record inserts one history row, fires the SSE event + audit entry and
// returns the new row id. rec.Kind defaults to the store kind.
func (s *HistoryStore) Record(rec *GenRecord) (int64, error) {
	if rec == nil {
		return 0, fmt.Errorf("HistoryStore.Record: rec required")
	}
	if rec.Kind == "" {
		rec.Kind = s.kind
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}
	extraJSON := "{}"
	if len(rec.Extra) > 0 {
		if b, err := json.Marshal(rec.Extra); err == nil {
			extraJSON = string(b)
		}
	}
	res, err := s.db.Exec(`INSERT INTO gen_history
		(kind, job_id, prompt, negative, preset, seed, model, engine, steps,
		 cfg_scale, sampler, scheduler, width, height, exec_ms, file_path,
		 thumb_path, storage_ref, ok, error, favorite, file_deleted,
		 extra_json, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0,?,?)`,
		rec.Kind, rec.JobID, rec.Prompt, rec.Negative, rec.Preset, rec.Seed,
		rec.Model, rec.Engine, rec.Steps, rec.CfgScale, rec.Sampler,
		rec.Scheduler, rec.Width, rec.Height, rec.ExecMS, rec.FilePath,
		rec.ThumbPath, rec.StorageRef, boolInt(rec.OK), rec.Error,
		extraJSON, rec.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("HistoryStore.Record: %w", err)
	}
	rec.ID, _ = res.LastInsertId()
	PublishEvent("history", map[string]any{"op": "add", "id": rec.ID, "kind": rec.Kind})
	Audit("history.add", rec.Model, fmt.Sprintf("id=%d ok=%v", rec.ID, rec.OK))
	// Auto-push to the storage box when the user enabled it (best-effort,
	// async - a slow/offline storage box must never block a render).
	if cfg := s.Config(); cfg.StorageAutoPush && rec.OK && rec.FilePath != "" {
		id := rec.ID
		go func() { _, _ = s.PushToStorage(id, "") }()
	}
	return rec.ID, nil
}

// PushToStorage sends a row's file to the storage box (cross-box
// /api/cb/save) and records the resulting ref ("profileID:path") on the row.
// profileID "" uses the configured default. Returns the ref.
func (s *HistoryStore) PushToStorage(id int64, profileID string) (string, error) {
	rec, err := s.Get(id)
	if err != nil {
		return "", err
	}
	if rec.FilePath == "" || rec.FileDeleted {
		return "", fmt.Errorf("history: no file to push")
	}
	cfg := s.Config()
	if profileID == "" {
		profileID = cfg.StorageProfileID
	}
	if profileID == "" {
		return "", fmt.Errorf("history: storage profile not configured")
	}
	abs, err := s.ResolveFile(rec.FilePath)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	dest := path.Base(filepath.ToSlash(rec.FilePath))
	if p := strings.Trim(strings.TrimSpace(cfg.StoragePathPrefix), "/"); p != "" {
		dest = p + "/" + dest
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	q := url.Values{"profile_id": {profileID}, "path": {dest}}
	resp, err := CallBox(ctx, StorageBoxID, http.MethodPost, "/api/cb/save?"+q.Encode(), f, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("history: storage push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("history: storage push: HTTP %d", resp.StatusCode)
	}
	ref := profileID + ":" + dest
	if err := s.SetStorageRef(id, ref); err != nil {
		return ref, err
	}
	PublishEvent("history", map[string]any{"op": "storage", "id": id})
	Audit("history.storage_push", dest, fmt.Sprintf("id=%d profile=%s", id, profileID))
	return ref, nil
}

// SetStorageRef marks a row as also pushed to the storage box.
func (s *HistoryStore) SetStorageRef(id int64, ref string) error {
	_, err := s.db.Exec(`UPDATE gen_history SET storage_ref = ? WHERE id = ?`, ref, id)
	return err
}

// HistoryFilter narrows List results. Zero values are ignored.
type HistoryFilter struct {
	Query        string // substring match on prompt
	Model        string
	FavoriteOnly bool
	OKOnly       bool
	Limit        int // default 60
	Offset       int
}

// List returns newest-first rows plus the total row count for the filter.
func (s *HistoryStore) List(f HistoryFilter) ([]GenRecord, int, error) {
	where := []string{"kind = ?"}
	args := []any{s.kind}
	if f.Query != "" {
		where = append(where, "prompt LIKE ?")
		args = append(args, "%"+f.Query+"%")
	}
	if f.Model != "" {
		where = append(where, "model = ?")
		args = append(args, f.Model)
	}
	if f.FavoriteOnly {
		where = append(where, "favorite = 1")
	}
	if f.OKOnly {
		where = append(where, "ok = 1")
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM gen_history WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 60
	}
	rows, err := s.db.Query(`SELECT id, kind, job_id, prompt, negative, preset,
		seed, model, engine, steps, cfg_scale, sampler, scheduler, width,
		height, exec_ms, file_path, thumb_path, storage_ref, ok, error,
		favorite, file_deleted, extra_json, created_at
		FROM gen_history WHERE `+cond+`
		ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []GenRecord{}
	for rows.Next() {
		if r, err := scanGenRecord(rows); err == nil {
			out = append(out, r)
		}
	}
	return out, total, nil
}

// Get returns one row by id (scoped to the store kind).
func (s *HistoryStore) Get(id int64) (GenRecord, error) {
	row := s.db.QueryRow(`SELECT id, kind, job_id, prompt, negative, preset,
		seed, model, engine, steps, cfg_scale, sampler, scheduler, width,
		height, exec_ms, file_path, thumb_path, storage_ref, ok, error,
		favorite, file_deleted, extra_json, created_at
		FROM gen_history WHERE id = ? AND kind = ?`, id, s.kind)
	return scanGenRecord(row)
}

type genRowScanner interface{ Scan(dest ...any) error }

func scanGenRecord(r genRowScanner) (GenRecord, error) {
	var g GenRecord
	var ok, fav, del int
	var extraJSON string
	err := r.Scan(&g.ID, &g.Kind, &g.JobID, &g.Prompt, &g.Negative, &g.Preset,
		&g.Seed, &g.Model, &g.Engine, &g.Steps, &g.CfgScale, &g.Sampler,
		&g.Scheduler, &g.Width, &g.Height, &g.ExecMS, &g.FilePath,
		&g.ThumbPath, &g.StorageRef, &ok, &g.Error, &fav, &del, &extraJSON,
		&g.CreatedAt)
	if err != nil {
		return g, err
	}
	g.OK, g.Favorite, g.FileDeleted = ok == 1, fav == 1, del == 1
	if extraJSON != "" && extraJSON != "{}" {
		_ = json.Unmarshal([]byte(extraJSON), &g.Extra)
	}
	return g, nil
}

// SetFavorite toggles the star. Favorites are exempt from retention.
func (s *HistoryStore) SetFavorite(id int64, fav bool) error {
	_, err := s.db.Exec(`UPDATE gen_history SET favorite = ? WHERE id = ? AND kind = ?`,
		boolInt(fav), id, s.kind)
	if err == nil {
		PublishEvent("history", map[string]any{"op": "favorite", "id": id})
	}
	return err
}

// Delete removes the row and (best-effort) its files.
func (s *HistoryStore) Delete(id int64) error {
	rec, err := s.Get(id)
	if err != nil {
		return err
	}
	s.removeFiles(rec)
	if _, err := s.db.Exec(`DELETE FROM gen_history WHERE id = ? AND kind = ?`, id, s.kind); err != nil {
		return err
	}
	PublishEvent("history", map[string]any{"op": "delete", "id": id})
	Audit("history.delete", rec.Model, fmt.Sprintf("id=%d", id))
	return nil
}

// ResolveFile returns the absolute path for a row's file or thumb, guarding
// against path traversal outside the outputs root.
func (s *HistoryStore) ResolveFile(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("no file")
	}
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	clean, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	rootAbs, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(clean, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes outputs root")
	}
	return clean, nil
}

func (s *HistoryStore) removeFiles(rec GenRecord) {
	for _, rel := range []string{rec.FilePath, rec.ThumbPath} {
		if rel == "" {
			continue
		}
		if abs, err := s.ResolveFile(rel); err == nil {
			_ = os.Remove(abs)
		}
	}
}

// HistoryStats feeds dashboard KPI tiles.
type HistoryStats struct {
	Total       int `json:"total"`
	Today       int `json:"today"`       // rolling 24h, ok only
	FailedToday int `json:"failedToday"` // rolling 24h
}

// Stats counts rows for the store kind. Boxes that had a legacy counter
// table can add its totals on top for continuity.
func (s *HistoryStore) Stats() HistoryStats {
	var st HistoryStats
	dayAgo := time.Now().Add(-24 * time.Hour).Unix()
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM gen_history WHERE kind = ? AND ok = 1`, s.kind).Scan(&st.Total)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM gen_history WHERE kind = ? AND ok = 1 AND created_at >= ?`, s.kind, dayAgo).Scan(&st.Today)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM gen_history WHERE kind = ? AND ok = 0 AND created_at >= ?`, s.kind, dayAgo).Scan(&st.FailedToday)
	return st
}

// Retention returns the stored policy (zero = unlimited).
func (s *HistoryStore) Retention() HistoryRetention {
	var r HistoryRetention
	if v := s.setting("retention"); v != "" {
		_ = json.Unmarshal([]byte(v), &r)
	}
	return r
}

// SetRetention persists the policy.
func (s *HistoryStore) SetRetention(r HistoryRetention) error {
	b, _ := json.Marshal(r)
	return s.setSetting("retention", string(b))
}

// RunRetention applies the policy once: deletes files (not rows) of
// non-favorite entries that exceed the age limit, then oldest-first until
// total size fits the MB limit. Rows are kept with file_deleted=1.
func (s *HistoryStore) RunRetention() {
	pol := s.Retention()
	if pol.MaxAgeDays <= 0 && pol.MaxTotalMB <= 0 {
		return
	}
	rows, err := s.db.Query(`SELECT id, file_path, thumb_path, created_at
		FROM gen_history
		WHERE kind = ? AND favorite = 0 AND file_deleted = 0 AND file_path != ''
		ORDER BY created_at ASC`, s.kind)
	if err != nil {
		return
	}
	type cand struct {
		id        int64
		file, thu string
		created   int64
		size      int64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.file, &c.thu, &c.created) != nil {
			continue
		}
		if abs, err := s.ResolveFile(c.file); err == nil {
			if fi, err := os.Stat(abs); err == nil {
				c.size = fi.Size()
			}
		}
		cands = append(cands, c)
	}
	rows.Close()

	drop := map[int64]cand{}
	if pol.MaxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -pol.MaxAgeDays).Unix()
		for _, c := range cands {
			if c.created < cutoff {
				drop[c.id] = c
			}
		}
	}
	if pol.MaxTotalMB > 0 {
		var total int64
		for _, c := range cands {
			if _, gone := drop[c.id]; !gone {
				total += c.size
			}
		}
		limit := int64(pol.MaxTotalMB) * 1024 * 1024
		if total > limit {
			// oldest first (cands already ASC by created_at)
			remaining := make([]cand, 0, len(cands))
			for _, c := range cands {
				if _, gone := drop[c.id]; !gone {
					remaining = append(remaining, c)
				}
			}
			sort.SliceStable(remaining, func(i, j int) bool { return remaining[i].created < remaining[j].created })
			for _, c := range remaining {
				if total <= limit {
					break
				}
				drop[c.id] = c
				total -= c.size
			}
		}
	}
	for id, c := range drop {
		s.removeFiles(GenRecord{FilePath: c.file, ThumbPath: c.thu})
		_, _ = s.db.Exec(`UPDATE gen_history SET file_deleted = 1 WHERE id = ?`, id)
	}
	if len(drop) > 0 {
		PublishEvent("history", map[string]any{"op": "retention", "removed": len(drop)})
		Audit("history.retention", "", fmt.Sprintf("removed %d files", len(drop)))
	}
}

// StartRetentionLoop runs RunRetention now and then every interval.
// Call once from main; returns immediately.
func (s *HistoryStore) StartRetentionLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	go func() {
		s.RunRetention()
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			s.RunRetention()
		}
	}()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
