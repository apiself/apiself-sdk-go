package sdk

// Globálne klávesové skratky pre boxy - zdieľaná SDK feature (ako AgentStore
// / Audit). Box deklaruje pomenované AKCIE (s handler-mi), user si k nim
// priradí ľubovoľné kombinácie (BINDINGy) cez UI. Skratky sú OS-level -
// fungujú aj keď nie je okno boxu v popredí, dokým box beží.
//
// Vlastníctvo: každý box má vlastné skratky vo svojej DB. Manager do toho
// nezasahuje (skratka beží v procese boxu).
//
// Platforma: Windows reálne (RegisterHotKey, CGO-free), macOS/Linux zatiaľ
// stub (Supported=false) — viď hotkey_windows.go / hotkey_other.go.
//
// Mount v box main.go:
//
//	hk, _ := sdk.NewHotkeyStore(store.RawDB())
//	hk.RegisterAction("record.toggle", "t:hotkey.action.record_toggle", func() { ... })
//	sdk.RegisterHotkeyEndpoints(mux, hk)
//	hk.Start()  // zaregistruje uložené bindingy
//
// ⚠ Swagger sync: /api/hotkeys* sú SDK-mounted (SDK_INJECTED_PATHS).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HotkeyAction je pomenovaná akcia ktorú box vystavuje na priradenie skratky.
type HotkeyAction struct {
	ID    string `json:"id"`    // napr. "record.toggle"
	Label string `json:"label"` // human/`t:` key pre UI
}

// HotkeyBinding je user-konfigurovaná kombinácia -> akcia.
type HotkeyBinding struct {
	ID      string `json:"id"`
	Action  string `json:"action"`  // ID registrovanej akcie
	Combo   string `json:"combo"`   // "ctrl+shift+r"
	Enabled bool   `json:"enabled"`
}

type hotkeyActionEntry struct {
	label   string
	handler func()
}

// HotkeyStore drží akcie (s handlermi), bindingy (v DB) a aktívne OS
// registrácie. Goroutine-safe.
type HotkeyStore struct {
	db      *sql.DB
	mu      sync.Mutex
	actions map[string]hotkeyActionEntry
	handles map[string]*hotkeyHandle // bindingID -> aktívna registrácia
	started bool
}

// NewHotkeyStore vytvorí tabuľku ak chýba a vráti store.
func NewHotkeyStore(db *sql.DB) (*HotkeyStore, error) {
	if db == nil {
		return nil, fmt.Errorf("HotkeyStore: db required")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS hotkeys (
		id         TEXT PRIMARY KEY,
		action     TEXT NOT NULL,
		combo      TEXT NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		created_at INTEGER NOT NULL
	)`); err != nil {
		return nil, fmt.Errorf("HotkeyStore: migrate: %w", err)
	}
	return &HotkeyStore{
		db:      db,
		actions: map[string]hotkeyActionEntry{},
		handles: map[string]*hotkeyHandle{},
	}, nil
}

// NewHotkeyStoreDefault otvorí (alebo vytvorí) dedikovanú DB na
// {BoxDataDir}/db/hotkeys.db a vráti store. Pre boxy bez vlastnej SQLite
// (napr. recorder) - nulový DB plumbing.
func NewHotkeyStoreDefault(boxID string) (*HotkeyStore, error) {
	path, err := BoxDBPath(boxID, "hotkeys.db")
	if err != nil {
		return nil, fmt.Errorf("NewHotkeyStoreDefault: %w", err)
	}
	db, err := OpenBoxSQLite(path, nil)
	if err != nil {
		return nil, fmt.Errorf("NewHotkeyStoreDefault: %w", err)
	}
	return NewHotkeyStore(db)
}

// RegisterAction deklaruje akciu + jej handler. Volaj pred Start().
func (s *HotkeyStore) RegisterAction(id, label string, handler func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions[id] = hotkeyActionEntry{label: label, handler: handler}
}

// Actions vráti zoznam deklarovaných akcií (pre UI dropdown).
func (s *HotkeyStore) Actions() []HotkeyAction {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HotkeyAction, 0, len(s.actions))
	for id, e := range s.actions {
		out = append(out, HotkeyAction{ID: id, Label: e.label})
	}
	return out
}

// Supported vráti či platforma podporuje globálne skratky.
func (s *HotkeyStore) Supported() bool { return hotkeySupported }

// ListBindings vráti všetky uložené bindingy.
func (s *HotkeyStore) ListBindings() ([]HotkeyBinding, error) {
	rows, err := s.db.Query(`SELECT id, action, combo, enabled FROM hotkeys ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HotkeyBinding{}
	for rows.Next() {
		var b HotkeyBinding
		var en int
		if err := rows.Scan(&b.ID, &b.Action, &b.Combo, &en); err != nil {
			return nil, err
		}
		b.Enabled = en != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddBinding vytvorí nový binding (enabled). Akcia musí existovať.
func (s *HotkeyStore) AddBinding(action, combo string) (*HotkeyBinding, error) {
	action = strings.TrimSpace(action)
	combo = strings.TrimSpace(combo)
	if action == "" || combo == "" {
		return nil, fmt.Errorf("action and combo required")
	}
	s.mu.Lock()
	_, ok := s.actions[action]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	b := &HotkeyBinding{ID: "hk-" + newInstanceID(), Action: action, Combo: combo, Enabled: true}
	if _, err := s.db.Exec(`INSERT INTO hotkeys (id, action, combo, enabled, created_at) VALUES (?, ?, ?, 1, ?)`,
		b.ID, action, combo, time.Now().Unix()); err != nil {
		return nil, err
	}
	s.reapply()
	return b, nil
}

// UpdateBinding zmení combo / enabled.
func (s *HotkeyStore) UpdateBinding(id, combo string, enabled bool) error {
	combo = strings.TrimSpace(combo)
	en := 0
	if enabled {
		en = 1
	}
	res, err := s.db.Exec(`UPDATE hotkeys SET combo = ?, enabled = ? WHERE id = ?`, combo, en, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("binding %s not found", id)
	}
	s.reapply()
	return nil
}

// DeleteBinding zmaže binding.
func (s *HotkeyStore) DeleteBinding(id string) error {
	if _, err := s.db.Exec(`DELETE FROM hotkeys WHERE id = ?`, id); err != nil {
		return err
	}
	s.reapply()
	return nil
}

// Start zaregistruje všetky enabled bindingy. Volaj raz po RegisterAction-och.
func (s *HotkeyStore) Start() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	s.reapply()
}

// reapply odregistruje všetko a nanovo zaregistruje enabled bindingy ktorých
// akcia existuje. Bezpečné volať pri každej zmene.
func (s *HotkeyStore) reapply() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || !hotkeySupported {
		return
	}
	for id, h := range s.handles {
		h.Stop()
		delete(s.handles, id)
	}
	bindings, err := s.listBindingsLocked()
	if err != nil {
		return
	}
	for _, b := range bindings {
		if !b.Enabled {
			continue
		}
		entry, ok := s.actions[b.Action]
		if !ok {
			continue
		}
		handler := entry.handler
		h, err := registerGlobalHotkey(b.Combo, func() { handler() })
		if err != nil {
			Log.Warn("hotkey.register_failed", "combo", b.Combo, "action", b.Action, "err", err.Error())
			continue
		}
		s.handles[b.ID] = h
	}
}

func (s *HotkeyStore) listBindingsLocked() ([]HotkeyBinding, error) {
	rows, err := s.db.Query(`SELECT id, action, combo, enabled FROM hotkeys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HotkeyBinding{}
	for rows.Next() {
		var b HotkeyBinding
		var en int
		if err := rows.Scan(&b.ID, &b.Action, &b.Combo, &en); err != nil {
			return nil, err
		}
		b.Enabled = en != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// ─── HTTP endpointy ──────────────────────────────────────────────────────────

// RegisterHotkeyEndpoints namountuje:
//
//	GET    /api/hotkeys           -> { supported, actions[], bindings[] }
//	POST   /api/hotkeys           { action, combo }
//	PUT    /api/hotkeys/{id}      { combo, enabled }
//	DELETE /api/hotkeys/{id}
func RegisterHotkeyEndpoints(mux *http.ServeMux, store *HotkeyStore) {
	h := &hotkeyHandlers{store: store}
	mux.HandleFunc("/api/hotkeys", h.collection)
	mux.HandleFunc("/api/hotkeys/", h.item)
}

type hotkeyHandlers struct{ store *HotkeyStore }

func (h *hotkeyHandlers) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
}
func (h *hotkeyHandlers) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": msg})
}

func (h *hotkeyHandlers) collection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		bindings, err := h.store.ListBindings()
		if err != nil {
			h.fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.ok(w, map[string]any{
			"supported": h.store.Supported(),
			"actions":   h.store.Actions(),
			"bindings":  bindings,
		})
	case http.MethodPost:
		var body struct{ Action, Combo string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		b, err := h.store.AddBinding(body.Action, body.Combo)
		if err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, b)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *hotkeyHandlers) item(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/hotkeys/")
	if id == "" || strings.Contains(id, "/") {
		h.fail(w, http.StatusBadRequest, "hotkey id required")
		return
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Combo   string `json:"combo"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "invalid body")
			return
		}
		if err := h.store.UpdateBinding(id, body.Combo, body.Enabled); err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, map[string]any{"id": id})
	case http.MethodDelete:
		if err := h.store.DeleteBinding(id); err != nil {
			h.fail(w, http.StatusBadRequest, err.Error())
			return
		}
		h.ok(w, map[string]any{"id": id})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
