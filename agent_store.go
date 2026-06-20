package sdk

// AgentStore - DB-backed store pre Box Agent Panel (in-box AI asistent).
//
// Spec: docs/box-agent-panel-spec.md. Každý box ktorý zapne agent panel
// drží svojich agentov + históriu spustení + settings v troch tabuľkách
// v box DB. Schéma je identická naprieč boxmi; tento helper poskytuje
// boilerplate, box len napojí svoj *sql.DB.
//
// Tri zdroje agentov:
//   - "seed" - ukážkoví agenti z .apiself/agents.json, importovaní pri
//              prvom štarte cez SeedFromFile. Label/prompt sú `t:` locale
//              keys. Re-import len keď sa zmení agents.json version.
//   - "user" - vytvorení userom cez box UI. Label/prompt raw text (jazyk
//              usera). Survive agents.json zmeny.
//
// agent_runs = audit + re-run história (rozhodnutie #6 v spec-e).
// agent_settings = key/value config (max_iterations, ...) (rozhodnutie #3).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Agent je jeden uložený recept (seed alebo user-created).
type Agent struct {
	ID        string `json:"id"`
	Source    string `json:"source"` // "seed" | "user"
	Label     string `json:"label"`  // seed: t: key; user: raw text
	Prompt    string `json:"prompt"`
	Icon      string `json:"icon,omitempty"`
	Tier      string `json:"tier"` // "free" | "basic" | "pro"
	SortOrder int    `json:"sortOrder"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// AgentRun je jeden záznam spustenia (audit / re-run).
type AgentRun struct {
	ID         string `json:"id"`
	AgentID    string `json:"agentId,omitempty"` // "" pre ad-hoc voľný text
	Prompt     string `json:"prompt"`
	PlanJSON   string `json:"planJson,omitempty"`  // navrhnutý plán
	StepsJSON  string `json:"stepsJson,omitempty"` // vykonané tool cally + výsledky
	Status     string `json:"status"`              // completed | failed | cancelled
	ResultText string `json:"resultText,omitempty"`
	ModelUsed  string `json:"modelUsed,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
}

// AgentStore je helper handle. Konštruovaný raz pri štarte boxu s box
// *sql.DB; goroutine-safe cez underlying sql.DB.
type AgentStore struct {
	db *sql.DB
}

// agentsSeedFile je očakávaný tvar .apiself/agents.json.
type agentsSeedFile struct {
	Version           int            `json:"version"`
	Agents            []agentSeedRow `json:"agents"`
	CrossBoxTools     []string       `json:"crossBoxTools,omitempty"`
	SystemPromptExtra string         `json:"systemPromptExtra,omitempty"`
}

type agentSeedRow struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Prompt string `json:"prompt"`
	Icon   string `json:"icon,omitempty"`
	Tier   string `json:"tier,omitempty"`
}

// NewAgentStore vytvorí tabuľky ak chýbajú a vráti helper.
func NewAgentStore(db *sql.DB) (*AgentStore, error) {
	if db == nil {
		return nil, fmt.Errorf("AgentStore: db required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agents (
			id          TEXT PRIMARY KEY,
			source      TEXT NOT NULL,
			label       TEXT NOT NULL,
			prompt      TEXT NOT NULL,
			icon        TEXT NOT NULL DEFAULT '',
			tier        TEXT NOT NULL DEFAULT 'free',
			sort_order  INTEGER NOT NULL DEFAULT 0,
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agents_source ON agents(source);

		CREATE TABLE IF NOT EXISTS agent_runs (
			id           TEXT PRIMARY KEY,
			agent_id     TEXT NOT NULL DEFAULT '',
			prompt       TEXT NOT NULL,
			plan_json    TEXT NOT NULL DEFAULT '',
			steps_json   TEXT NOT NULL DEFAULT '',
			status       TEXT NOT NULL,
			result_text  TEXT NOT NULL DEFAULT '',
			model_used   TEXT NOT NULL DEFAULT '',
			created_at   INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_runs_created ON agent_runs(created_at DESC);

		CREATE TABLE IF NOT EXISTS agent_settings (
			key    TEXT PRIMARY KEY,
			value  TEXT NOT NULL
		);
	`); err != nil {
		return nil, fmt.Errorf("AgentStore: migrate: %w", err)
	}
	return &AgentStore{db: db}, nil
}

// ─── Seed import ─────────────────────────────────────────────────────────────

// SeedFromFile naimportuje ukážkových agentov z .apiself/agents.json.
// Idempotentné: seed agentov UPSERT-uje (label/prompt/icon/tier z file-u
// sú autoritatívne pri každom štarte), user agentov sa nedotkne. Seed
// agent ktorý zmizol z agents.json sa odstráni (GC, len source='seed').
//
// Bezpečné volať pri každom box štarte. Chýbajúci súbor = no-op (box bez
// agentov). Vráti počet seed agentov + cross-box allowlist + extra system
// prompt (box ich postúpi do UI cez settings endpoint).
func (s *AgentStore) SeedFromFile(path string) (crossBoxTools []string, systemPromptExtra string, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, "", nil // box bez agentov - nie error
		}
		return nil, "", fmt.Errorf("AgentStore.SeedFromFile: read: %w", readErr)
	}
	var f agentsSeedFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, "", fmt.Errorf("AgentStore.SeedFromFile: parse: %w", err)
	}

	now := time.Now().Unix()
	keep := make(map[string]bool, len(f.Agents))
	for i, a := range f.Agents {
		if a.ID == "" {
			continue
		}
		keep[a.ID] = true
		tier := a.Tier
		if tier == "" {
			tier = "free"
		}
		if _, err := s.db.Exec(`
			INSERT INTO agents (id, source, label, prompt, icon, tier, sort_order, created_at, updated_at)
			VALUES (?, 'seed', ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label      = excluded.label,
				prompt     = excluded.prompt,
				icon       = excluded.icon,
				tier       = excluded.tier,
				sort_order = excluded.sort_order,
				updated_at = excluded.updated_at
			WHERE agents.source = 'seed'`,
			a.ID, a.Label, a.Prompt, a.Icon, tier, i, now, now,
		); err != nil {
			return nil, "", fmt.Errorf("AgentStore.SeedFromFile: upsert %s: %w", a.ID, err)
		}
	}

	// Ulož cross-box allowlist + extra system prompt do settings, aby ich
	// UI dostalo cez jeden /api/agents/settings fetch (netreba parsovať
	// agents.json na klientovi).
	cbJSON, _ := json.Marshal(f.CrossBoxTools)
	_, _ = s.db.Exec(`INSERT INTO agent_settings (key, value) VALUES ('cross_box_tools', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(cbJSON))
	_, _ = s.db.Exec(`INSERT INTO agent_settings (key, value) VALUES ('system_prompt_extra', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, f.SystemPromptExtra)

	// GC: seed agenti ktorí už nie sú v agents.json (user agentov sa
	// nedotkneme). Ak je agents.json prázdny, GC preskočíme - skoro
	// určite parsing bug, nie zámer zmazať všetkých.
	if len(keep) > 0 {
		rows, qerr := s.db.Query(`SELECT id FROM agents WHERE source = 'seed'`)
		if qerr == nil {
			var stale []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil && !keep[id] {
					stale = append(stale, id)
				}
			}
			rows.Close()
			for _, id := range stale {
				_, _ = s.db.Exec(`DELETE FROM agents WHERE id = ? AND source = 'seed'`, id)
			}
		}
	}

	return f.CrossBoxTools, f.SystemPromptExtra, nil
}

// ─── Agents CRUD ─────────────────────────────────────────────────────────────

// ListAgents vráti všetkých agentov: seed prvé (podľa sort_order), user
// druhé (podľa created_at). UI ich renderuje ako tlačidlá.
func (s *AgentStore) ListAgents() ([]Agent, error) {
	rows, err := s.db.Query(`
		SELECT id, source, label, prompt, icon, tier, sort_order, created_at, updated_at
		  FROM agents
		 ORDER BY
		   CASE source WHEN 'seed' THEN 0 ELSE 1 END,
		   sort_order ASC,
		   created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.ID, &a.Source, &a.Label, &a.Prompt, &a.Icon,
			&a.Tier, &a.SortOrder, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateUserAgent vytvorí user agenta. ID sa vygeneruje (user nezadáva).
func (s *AgentStore) CreateUserAgent(label, prompt, icon, tier string) (*Agent, error) {
	if strings.TrimSpace(label) == "" || strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("AgentStore.CreateUserAgent: label and prompt required")
	}
	if tier == "" {
		tier = "free"
	}
	now := time.Now().Unix()
	id := "user-" + newInstanceID()
	if _, err := s.db.Exec(`
		INSERT INTO agents (id, source, label, prompt, icon, tier, sort_order, created_at, updated_at)
		VALUES (?, 'user', ?, ?, ?, ?, 0, ?, ?)`,
		id, label, prompt, icon, tier, now, now,
	); err != nil {
		return nil, err
	}
	return &Agent{ID: id, Source: "user", Label: label, Prompt: prompt,
		Icon: icon, Tier: tier, CreatedAt: now, UpdatedAt: now}, nil
}

// UpdateUserAgent upraví user agenta. Seed agentov nemodifikuje (vráti
// chybu - tie sa menia len cez agents.json).
func (s *AgentStore) UpdateUserAgent(id, label, prompt, icon, tier string) error {
	if tier == "" {
		tier = "free"
	}
	res, err := s.db.Exec(`
		UPDATE agents
		   SET label = ?, prompt = ?, icon = ?, tier = ?, updated_at = ?
		 WHERE id = ? AND source = 'user'`,
		label, prompt, icon, tier, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("AgentStore.UpdateUserAgent: %s not found or is a seed agent", id)
	}
	return nil
}

// DeleteUserAgent zmaže user agenta. Seed agentov nemaže.
func (s *AgentStore) DeleteUserAgent(id string) error {
	res, err := s.db.Exec(`DELETE FROM agents WHERE id = ? AND source = 'user'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("AgentStore.DeleteUserAgent: %s not found or is a seed agent", id)
	}
	return nil
}

// ─── Runs (audit / re-run) ───────────────────────────────────────────────────

// SaveRun uloží záznam spustenia. ID sa vygeneruje ak je prázdne.
func (s *AgentStore) SaveRun(r AgentRun) (string, error) {
	if r.ID == "" {
		r.ID = "run-" + newInstanceID()
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().Unix()
	}
	if r.Status == "" {
		r.Status = "completed"
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_runs (id, agent_id, prompt, plan_json, steps_json, status, result_text, model_used, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.AgentID, r.Prompt, r.PlanJSON, r.StepsJSON, r.Status, r.ResultText, r.ModelUsed, r.CreatedAt)
	return r.ID, err
}

// ListRuns vráti posledných `limit` spustení (najnovšie prvé). limit<=0
// → default 50.
func (s *AgentStore) ListRuns(limit int) ([]AgentRun, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, agent_id, prompt, plan_json, steps_json, status, result_text, model_used, created_at
		  FROM agent_runs
		 ORDER BY created_at DESC
		 LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgentRun{}
	for rows.Next() {
		var r AgentRun
		if err := rows.Scan(&r.ID, &r.AgentID, &r.Prompt, &r.PlanJSON, &r.StepsJSON,
			&r.Status, &r.ResultText, &r.ModelUsed, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun vráti jeden run podľa ID alebo (nil, nil) keď neexistuje.
func (s *AgentStore) GetRun(id string) (*AgentRun, error) {
	var r AgentRun
	err := s.db.QueryRow(`
		SELECT id, agent_id, prompt, plan_json, steps_json, status, result_text, model_used, created_at
		  FROM agent_runs WHERE id = ? LIMIT 1`, id).Scan(
		&r.ID, &r.AgentID, &r.Prompt, &r.PlanJSON, &r.StepsJSON,
		&r.Status, &r.ResultText, &r.ModelUsed, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ─── Settings (max iterations, ...) ──────────────────────────────────────────

const defaultMaxIterations = 8

// MaxIterations vráti uložený limit alebo default 8. Env
// APISELF_AGENT_MAX_ITER má prednosť (dev override).
func (s *AgentStore) MaxIterations() int {
	if env := os.Getenv("APISELF_AGENT_MAX_ITER"); env != "" {
		if n, err := strconv.Atoi(env); err == nil && n > 0 {
			return n
		}
	}
	var v string
	err := s.db.QueryRow(`SELECT value FROM agent_settings WHERE key = 'max_iterations'`).Scan(&v)
	if err == nil {
		if n, perr := strconv.Atoi(v); perr == nil && n > 0 {
			return n
		}
	}
	return defaultMaxIterations
}

// SetMaxIterations uloží limit. n<=0 → default.
func (s *AgentStore) SetMaxIterations(n int) error {
	if n <= 0 {
		n = defaultMaxIterations
	}
	_, err := s.db.Exec(`
		INSERT INTO agent_settings (key, value) VALUES ('max_iterations', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		strconv.Itoa(n))
	return err
}

// CrossBoxTools vráti allowlist boxov ktorých tools agenti smú volať
// (uložené pri SeedFromFile). Prázdne ak box agentov nemá / žiadny cross-box.
func (s *AgentStore) CrossBoxTools() []string {
	var v string
	if s.db.QueryRow(`SELECT value FROM agent_settings WHERE key = 'cross_box_tools'`).Scan(&v) != nil {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(v), &out) != nil {
		return nil
	}
	return out
}

// SystemPromptExtra vráti box-špecifický dodatok k system promptu agenta.
func (s *AgentStore) SystemPromptExtra() string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM agent_settings WHERE key = 'system_prompt_extra'`).Scan(&v)
	return v
}
