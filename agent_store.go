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

// AgentParam je jedno vstupné pole agenta (v2). Vyrenderuje sa ako
// formulár pred behom; hodnota sa vloží do user správy.
type AgentParam struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"` // raw text alebo t: key
	Type        string   `json:"type"`  // string | number | boolean | enum
	Required    bool     `json:"required,omitempty"`
	Options     []string `json:"options,omitempty"` // pre type=enum
	Placeholder string   `json:"placeholder,omitempty"`
}

// Agent je jeden uložený recept (seed alebo user-created).
//
// v1 polia (label/prompt/icon/tier) sú DB stĺpce; v2 polia
// (description..params) sa serializujú do `definition` JSON stĺpca - viď
// spec §16. Spätná kompatibilita: starý riadok = prázdne v2 polia.
type Agent struct {
	ID        string `json:"id"`
	Source    string `json:"source"` // "seed" | "user"
	Label     string `json:"label"`  // seed: t: key; user: raw text
	Prompt    string `json:"prompt"`
	Icon      string `json:"icon,omitempty"`
	Tier      string `json:"tier"` // "free" | "basic" | "pro"
	SortOrder int    `json:"sortOrder"`

	// ── v2 (definition JSON) ──
	Description   string       `json:"description,omitempty"`   // human popis, nikdy do LLM
	Instructions  string       `json:"instructions,omitempty"`  // per-agent system prompt
	Tools         []string     `json:"tools,omitempty"`         // allowlist operationId; [] = všetky vlastné
	CrossBoxTools []string     `json:"crossBoxTools,omitempty"` // ktoré iné boxy smie volať
	Model         string       `json:"model,omitempty"`         // pin modelu; "" = user výber
	Params        []AgentParam `json:"params,omitempty"`        // vstupný formulár

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// agentDefinition je v2 časť agenta uložená v `definition` JSON stĺpci.
type agentDefinition struct {
	Description   string       `json:"description,omitempty"`
	Instructions  string       `json:"instructions,omitempty"`
	Tools         []string     `json:"tools,omitempty"`
	CrossBoxTools []string     `json:"crossBoxTools,omitempty"`
	Model         string       `json:"model,omitempty"`
	Params        []AgentParam `json:"params,omitempty"`
}

func (a *Agent) definitionJSON() string {
	d := agentDefinition{
		Description: a.Description, Instructions: a.Instructions,
		Tools: a.Tools, CrossBoxTools: a.CrossBoxTools,
		Model: a.Model, Params: a.Params,
	}
	b, _ := json.Marshal(d)
	return string(b)
}

func (a *Agent) applyDefinition(raw string) {
	if raw == "" {
		return
	}
	var d agentDefinition
	if json.Unmarshal([]byte(raw), &d) != nil {
		return
	}
	a.Description, a.Instructions = d.Description, d.Instructions
	a.Tools, a.CrossBoxTools = d.Tools, d.CrossBoxTools
	a.Model, a.Params = d.Model, d.Params
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
	// v2
	Description   string       `json:"description,omitempty"`
	Instructions  string       `json:"instructions,omitempty"`
	Tools         []string     `json:"tools,omitempty"`
	CrossBoxTools []string     `json:"crossBoxTools,omitempty"`
	Model         string       `json:"model,omitempty"`
	Params        []AgentParam `json:"params,omitempty"`
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
			definition  TEXT NOT NULL DEFAULT '{}',
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
	// v2 upgrade pre staré DB (agents tabuľka bez definition stĺpca).
	// "duplicate column name" znamená že už existuje - ignorujeme.
	if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN definition TEXT NOT NULL DEFAULT '{}'`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("AgentStore: migrate v2: %w", err)
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
		def := (&Agent{
			Description: a.Description, Instructions: a.Instructions,
			Tools: a.Tools, CrossBoxTools: a.CrossBoxTools,
			Model: a.Model, Params: a.Params,
		}).definitionJSON()
		if _, err := s.db.Exec(`
			INSERT INTO agents (id, source, label, prompt, icon, tier, sort_order, definition, created_at, updated_at)
			VALUES (?, 'seed', ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				label      = excluded.label,
				prompt     = excluded.prompt,
				icon       = excluded.icon,
				tier       = excluded.tier,
				sort_order = excluded.sort_order,
				definition = excluded.definition,
				updated_at = excluded.updated_at
			WHERE agents.source = 'seed'`,
			a.ID, a.Label, a.Prompt, a.Icon, tier, i, def, now, now,
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
		SELECT id, source, label, prompt, icon, tier, sort_order, definition, created_at, updated_at
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
		var def string
		if err := rows.Scan(&a.ID, &a.Source, &a.Label, &a.Prompt, &a.Icon,
			&a.Tier, &a.SortOrder, &def, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.applyDefinition(def)
		out = append(out, a)
	}
	return out, rows.Err()
}

// CreateUserAgent vytvorí user agenta z v2 definície. ID sa vygeneruje
// (user nezadáva); source/timestamps sa nastavia interne.
func (s *AgentStore) CreateUserAgent(a Agent) (*Agent, error) {
	if strings.TrimSpace(a.Label) == "" || strings.TrimSpace(a.Prompt) == "" {
		return nil, fmt.Errorf("AgentStore.CreateUserAgent: label and prompt required")
	}
	if a.Tier == "" {
		a.Tier = "free"
	}
	now := time.Now().Unix()
	a.ID = "user-" + newInstanceID()
	a.Source = "user"
	a.CreatedAt, a.UpdatedAt = now, now
	if _, err := s.db.Exec(`
		INSERT INTO agents (id, source, label, prompt, icon, tier, sort_order, definition, created_at, updated_at)
		VALUES (?, 'user', ?, ?, ?, ?, 0, ?, ?, ?)`,
		a.ID, a.Label, a.Prompt, a.Icon, a.Tier, a.definitionJSON(), now, now,
	); err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateUserAgent upraví user agenta z v2 definície. Seed agentov
// nemodifikuje (vráti chybu - tie sa menia len cez agents.json).
func (s *AgentStore) UpdateUserAgent(id string, a Agent) error {
	if strings.TrimSpace(a.Label) == "" || strings.TrimSpace(a.Prompt) == "" {
		return fmt.Errorf("AgentStore.UpdateUserAgent: label and prompt required")
	}
	if a.Tier == "" {
		a.Tier = "free"
	}
	res, err := s.db.Exec(`
		UPDATE agents
		   SET label = ?, prompt = ?, icon = ?, tier = ?, definition = ?, updated_at = ?
		 WHERE id = ? AND source = 'user'`,
		a.Label, a.Prompt, a.Icon, a.Tier, a.definitionJSON(), time.Now().Unix(), id)
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

// DefaultSystemPrompt je východzie základné správanie asistenta (tool-use +
// anti-halucinácia). Editovateľné cez SetSystemPrompt; UI ho ukáže ako
// default v nastaveniach asistenta.
const DefaultSystemPrompt = "You are an assistant embedded in this app. You have tools that call the app's real API. " +
	"ALWAYS call the matching tool to read or change data — never invent, assume or guess data " +
	"(lists, counts, statuses, names, IDs). Base every factual statement ONLY on a tool result. " +
	"If a question needs data, call the tool first, then answer from what it returned. " +
	"Prefer the fewest tool calls needed. Respond in the user's language."

// SystemPrompt vráti uložený základný system prompt alebo default.
func (s *AgentStore) SystemPrompt() string {
	var v string
	if s.db.QueryRow(`SELECT value FROM agent_settings WHERE key = 'system_prompt'`).Scan(&v) == nil && strings.TrimSpace(v) != "" {
		return v
	}
	return DefaultSystemPrompt
}

// SetSystemPrompt uloží základný system prompt. Prázdny = reset na default.
func (s *AgentStore) SetSystemPrompt(p string) error {
	_, err := s.db.Exec(`INSERT INTO agent_settings (key, value) VALUES ('system_prompt', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, p)
	return err
}
