package sdk

// Standardizovaný operačný audit (aktivita) pre boxy. Zachytáva ČO box
// robil - stiahnutie modelu, pridanie poskytovateľa, agent beh, inštaláciu
// dependency, zmenu configu - do jednej uniformnej časovej osi, ktorú UI
// renderuje cez SDK <BoxActivityLog>.
//
// Zámerne oddelené od dvoch iných vrstiev:
//   - sdk.Log (slog -> stdout/manager.log)  = dev/ops logy (chyby, lifecycle)
//   - doménové tabuľky boxu (notify messages, tts klipy, ...) = primárny
//     obsah feature, vlastné view
// audit_log je user-facing "čo sa dialo" naprieč všetkými boxmi rovnako.
//
// Package-global (rovnako ako sdk.Log) - box ho zapne jedným callom v
// main.go a potom volá sdk.Audit(...) z hociktorého handlera bez
// threadovania store cez Ctx:
//
//	sdk.InitAudit(store.RawDB())
//	sdk.RegisterAuditEndpoints(mux)
//	...
//	sdk.Audit("model.install", id, displayName)
//
// ⚠ Swagger sync: /api/audit je SDK-mounted, vedené v
// scripts/check-swagger-sync.py SDK_INJECTED_PATHS. Box ho môže (ale
// nemusí) duplikovať vo swagger.json.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// AuditEvent je jeden záznam operačnej histórie boxu.
type AuditEvent struct {
	ID     int64  `json:"id"`
	At     int64  `json:"at"`     // unix sekundy
	Action string `json:"action"` // namespace.akcia, napr. model.install, provider.create
	Target string `json:"target"` // id / názov dotknutej entity
	Detail string `json:"detail"` // voľný text (veľkosť, host, počet, ...)
}

var (
	auditMu sync.RWMutex
	auditDB *sql.DB
)

// InitAudit vytvorí audit_log tabuľku a zaregistruje DB pre Audit().
// Volaj raz pri štarte s raw DB boxu. Idempotentné.
func InitAudit(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("InitAudit: nil db")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS audit_log (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		at      INTEGER NOT NULL,
		action  TEXT NOT NULL,
		target  TEXT NOT NULL DEFAULT '',
		detail  TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return fmt.Errorf("InitAudit: %w", err)
	}
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log(at DESC)`)
	auditMu.Lock()
	auditDB = db
	auditMu.Unlock()
	return nil
}

// InitAuditDefault otvorí (alebo vytvorí) dedikovanú audit DB boxu na
// {BoxDataDir}/db/audit.db a zaregistruje ju. Použi pre boxy ktoré nemajú
// vlastnú SQLite - sprístupní Aktivita tab všade s nulovým DB plumbingom.
// Boxy s vlastnou DB môžu radšej zavolať InitAudit(rawDB) a ušetriť druhý
// súbor. Volaj raz pri štarte.
func InitAuditDefault(boxID string) error {
	path, err := BoxDBPath(boxID, "audit.db")
	if err != nil {
		return fmt.Errorf("InitAuditDefault: %w", err)
	}
	db, err := OpenBoxSQLite(path, nil)
	if err != nil {
		return fmt.Errorf("InitAuditDefault: %w", err)
	}
	return InitAudit(db)
}

// Audit pripíše jednu udalosť. Best-effort: zlyhaný zápis (alebo
// neinicializovaný audit) sa ticho ignoruje - chýbajúci audit záznam nesmie
// nikdy rozbiť operáciu, ktorú audituje.
func Audit(action, target, detail string) {
	auditMu.RLock()
	db := auditDB
	auditMu.RUnlock()
	if db == nil {
		return
	}
	_, _ = db.Exec(
		`INSERT INTO audit_log (at, action, target, detail) VALUES (?, ?, ?, ?)`,
		time.Now().Unix(), action, target, detail,
	)
	// Push an "audit" event so every box's <BoxActivityLog> refreshes live
	// (no polling). One place covers every box's Activity tab uniformly.
	PublishEvent("audit", map[string]any{"action": action})
}

// ListAudit vráti N najnovších udalostí, najnovšie prvé.
func ListAudit(limit int) ([]AuditEvent, error) {
	auditMu.RLock()
	db := auditDB
	auditMu.RUnlock()
	if db == nil {
		return []AuditEvent{}, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := db.Query(
		`SELECT id, at, action, target, detail FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AuditEvent, 0, limit)
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.At, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RegisterAuditEndpoints namountuje GET /api/audit?limit=N ->
// { "success": true, "data": { "events": [...] } }.
func RegisterAuditEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/api/audit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "method not allowed"})
			return
		}
		limit := 200
		if q := r.URL.Query().Get("limit"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 1000 {
				limit = n
			}
		}
		events, err := ListAudit(limit)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"events": events},
		})
	})
}
