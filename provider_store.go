package sdk

// ProviderStore is the cloud (BYOK) half of the AI Studio standard
// (docs/box-ai-studio-spec.md). It mirrors ModelStore/AgentStore:
//
//   - "preset" - shipped in config.json providers.presets[], UPSERTed into
//                the `providers` table at every startup. adapter/label/
//                models/docs come from config (authoritative); a preset
//                row's encrypted key is preserved across re-seeds. A preset
//                that disappears from config is GC'd (only source='preset').
//   - "custom" - user-added through the box UI (own endpoint / self-hosted
//                gateway). Survives config changes. Only path besides
//                setting a key that mutates the catalogue at runtime.
//
// API keys are never in config.json - they are user-supplied, encrypted at
// rest via the SDK Vault (AES-GCM keyed from HWID) and stored in the
// key_ciphertext column. The plaintext only lives in RAM during a call.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Provider is one cloud provider row (or config preset before seeding).
type Provider struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`     // "preset" | "custom"
	Adapter    string   `json:"adapter"`    // CloudAdapter id
	Label      string   `json:"label"`
	Models     []string `json:"models"`
	DocsURL    string   `json:"docsUrl,omitempty"`
	Capability string   `json:"capability,omitempty"`
	Configured bool     `json:"configured"` // has an API key set (never exposes the key)
}

// ProviderStore persists the provider catalogue + encrypted keys.
type ProviderStore struct {
	db    *sql.DB
	vault *Vault
}

// NewProviderStore opens (creating if needed) the `providers` table on the
// given box DB. vault may be nil for boxes that only list providers without
// storing keys, but SetKey/Key then error.
func NewProviderStore(db *sql.DB, vault *Vault) (*ProviderStore, error) {
	if db == nil {
		return nil, fmt.Errorf("ProviderStore: db required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS providers (
			id             TEXT PRIMARY KEY,
			source         TEXT NOT NULL,
			adapter        TEXT NOT NULL DEFAULT '',
			label          TEXT NOT NULL DEFAULT '',
			models_json    TEXT NOT NULL DEFAULT '[]',
			docs_url       TEXT NOT NULL DEFAULT '',
			capability     TEXT NOT NULL DEFAULT '',
			key_ciphertext BLOB,
			created_at     INTEGER NOT NULL,
			updated_at     INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_providers_source ON providers(source);
	`); err != nil {
		return nil, fmt.Errorf("ProviderStore: create table: %w", err)
	}
	return &ProviderStore{db: db, vault: vault}, nil
}

// SeedFromConfig UPSERTs preset providers into the table. Idempotent:
// adapter/label/models/docs/capability from config are authoritative on
// every start; the encrypted key on a preset row is PRESERVED (COALESCE) so
// re-seeding never wipes a user's key. Custom rows are never touched. A
// preset that vanished from config is deleted (source='preset' only).
func (s *ProviderStore) SeedFromConfig(presets []Provider) error {
	now := time.Now().Unix()
	keep := make(map[string]bool, len(presets))
	for _, p := range presets {
		keep[p.ID] = true
		modelsJSON, _ := json.Marshal(p.Models)
		if _, err := s.db.Exec(`
			INSERT INTO providers (id, source, adapter, label, models_json, docs_url, capability, key_ciphertext, created_at, updated_at)
			VALUES (?, 'preset', ?, ?, ?, ?, ?, NULL, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				source      = 'preset',
				adapter     = excluded.adapter,
				label       = excluded.label,
				models_json = excluded.models_json,
				docs_url    = excluded.docs_url,
				capability  = excluded.capability,
				updated_at  = excluded.updated_at`,
			p.ID, p.Adapter, p.Label, string(modelsJSON), p.DocsURL, p.Capability, now, now); err != nil {
			return fmt.Errorf("ProviderStore.SeedFromConfig: upsert %s: %w", p.ID, err)
		}
	}
	// GC presets that are no longer in config (leaves custom rows alone).
	rows, err := s.db.Query(`SELECT id FROM providers WHERE source = 'preset'`)
	if err != nil {
		return nil
	}
	var stale []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !keep[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		_, _ = s.db.Exec(`DELETE FROM providers WHERE id = ? AND source = 'preset'`, id)
	}
	return nil
}

// List returns every provider row, presets first then customs, each with a
// Configured flag. Never returns the key material.
func (s *ProviderStore) List() ([]Provider, error) {
	rows, err := s.db.Query(`
		SELECT id, source, adapter, label, models_json, docs_url, capability,
		       key_ciphertext IS NOT NULL
		FROM providers
		ORDER BY CASE source WHEN 'preset' THEN 0 ELSE 1 END, label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		var p Provider
		var modelsJSON string
		if err := rows.Scan(&p.ID, &p.Source, &p.Adapter, &p.Label, &modelsJSON, &p.DocsURL, &p.Capability, &p.Configured); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(modelsJSON), &p.Models)
		if p.Models == nil {
			p.Models = []string{}
		}
		out = append(out, p)
	}
	return out, nil
}

// AddCustom inserts a user-defined provider (source='custom'). If an apiKey
// is supplied it is encrypted and stored in the same call.
func (s *ProviderStore) AddCustom(p Provider, apiKey string) error {
	now := time.Now().Unix()
	modelsJSON, _ := json.Marshal(p.Models)
	var ct []byte
	if apiKey != "" {
		if s.vault == nil {
			return fmt.Errorf("ProviderStore.AddCustom: vault required to store a key")
		}
		var err error
		if ct, err = s.vault.Encrypt(p.ID, apiKey); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`
		INSERT INTO providers (id, source, adapter, label, models_json, docs_url, capability, key_ciphertext, created_at, updated_at)
		VALUES (?, 'custom', ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			adapter     = excluded.adapter,
			label       = excluded.label,
			models_json = excluded.models_json,
			docs_url    = excluded.docs_url,
			capability  = excluded.capability,
			updated_at  = excluded.updated_at`,
		p.ID, p.Adapter, p.Label, string(modelsJSON), p.DocsURL, p.Capability, ct, now, now)
	return err
}

// SetKey encrypts and stores an API key for an existing provider row (preset
// or custom). Errors if the provider id is unknown.
func (s *ProviderStore) SetKey(id, apiKey string) error {
	if s.vault == nil {
		return fmt.Errorf("ProviderStore.SetKey: vault required")
	}
	ct, err := s.vault.Encrypt(id, apiKey)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE providers SET key_ciphertext = ?, updated_at = ? WHERE id = ?`, ct, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ProviderStore.SetKey: unknown provider %q", id)
	}
	return nil
}

// Remove deletes a custom provider row, or clears the key on a preset row
// (so a preset returns to "not configured" without vanishing from the list).
func (s *ProviderStore) Remove(id string) error {
	var source string
	if err := s.db.QueryRow(`SELECT source FROM providers WHERE id = ?`, id).Scan(&source); err != nil {
		return err
	}
	if source == "custom" {
		_, err := s.db.Exec(`DELETE FROM providers WHERE id = ? AND source = 'custom'`, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE providers SET key_ciphertext = NULL, updated_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// Key returns the decrypted API key for a provider, or ("", false) when no
// key is set. The plaintext must not be logged or persisted.
func (s *ProviderStore) Key(id string) (string, bool) {
	if s.vault == nil {
		return "", false
	}
	var ct []byte
	if err := s.db.QueryRow(`SELECT key_ciphertext FROM providers WHERE id = ?`, id).Scan(&ct); err != nil || ct == nil {
		return "", false
	}
	key, err := s.vault.Decrypt(id, ct)
	if err != nil {
		return "", false
	}
	return key, true
}

// Get returns a single provider row (Configured set), or (Provider{}, false).
func (s *ProviderStore) Get(id string) (Provider, bool) {
	var p Provider
	var modelsJSON string
	err := s.db.QueryRow(`
		SELECT id, source, adapter, label, models_json, docs_url, capability, key_ciphertext IS NOT NULL
		FROM providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Source, &p.Adapter, &p.Label, &modelsJSON, &p.DocsURL, &p.Capability, &p.Configured)
	if err != nil {
		return Provider{}, false
	}
	_ = json.Unmarshal([]byte(modelsJSON), &p.Models)
	return p, true
}
