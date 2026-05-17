package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuthConfig je per-box runtime auth nastavenie. Manager ho injektuje
// pri štarte cez env premenné; box si ho kešuje na disku ako JSON aby
// vedel svoj stav aj keby manager spadol.
//
// V0.8 zmena oproti v0.7: per-box `auth_required` toggle bol predtým
// v manager.db a frontend ho musel pýtať managera. Teraz box vlastní
// svoju kópiu — single-source-of-truth ostáva manager, ale cache je
// lokálna v `{BoxDataDir}/auth_config.json`.
//
// JSON file je zvolený namiesto SQLite tabuľky aby SDK nemuselo
// importovať sqlite3 driver — boxy ho síce používajú, ale SDK má byť
// driver-agnostic. 4 polia v JSON-e nestoja za to ťahať CGO dependency.
type AuthConfig struct {
	// Required — či tento konkrétny box vyžaduje prihlásenie. Default false.
	Required bool `json:"required"`

	// BoxURL — URL auth boxu (z env APISELF_AUTH_BOX_URL). Empty ak auth
	// box nie je nainštalovaný.
	BoxURL string `json:"box_url,omitempty"`

	// RegistrationMode — closed | open | approval | email_verify. Default
	// "closed" (invitation-only). Frontend to číta cez /api/info.
	RegistrationMode string `json:"registration_mode,omitempty"`

	// LastSyncedAt — kedy sa naposledy refresh-ovalo z env (unix ts).
	LastSyncedAt int64 `json:"last_synced_at,omitempty"`
}

// Env premenné ktoré manager injektuje pri spustení boxu.
const (
	EnvAuthRequired     = "APISELF_AUTH_REQUIRED"
	EnvAuthBoxURL       = "APISELF_AUTH_BOX_URL"
	EnvAuthRegistration = "APISELF_AUTH_REGISTRATION"
)

// authConfigCache drží načítaný config per-process. SyncAuthConfigFromEnv
// ho aktualizuje pri štarte, GetAuthConfig vracia kópiu.
var authConfigCache = struct {
	mu  sync.RWMutex
	cfg AuthConfig
}{}

// authConfigPath vráti cestu k JSON cache súboru pre daný box.
func authConfigPath(boxID string) string {
	return filepath.Join(BoxDataDir(boxID), "auth_config.json")
}

// SyncAuthConfigFromEnv číta env premenné a zapisuje výslednú konfiguráciu
// do {BoxDataDir}/auth_config.json. Volá sa interne z InitBox; box code
// to ručne nepotrebuje.
//
// Side-effects:
//   - prepíše JSON súbor s aktuálnymi env hodnotami
//   - aktualizuje authConfigCache
//
// Ak file write zlyhá, vráti error ale cache je aj tak naplnená — frontend
// dostane cez /api/info aspoň env hodnoty (in-memory).
func SyncAuthConfigFromEnv(boxID string) error {
	cfg := AuthConfig{
		Required:         os.Getenv(EnvAuthRequired) == "true",
		BoxURL:           os.Getenv(EnvAuthBoxURL),
		RegistrationMode: os.Getenv(EnvAuthRegistration),
		LastSyncedAt:     time.Now().Unix(),
	}
	if cfg.RegistrationMode == "" {
		cfg.RegistrationMode = "closed"
	}

	authConfigCache.mu.Lock()
	authConfigCache.cfg = cfg
	authConfigCache.mu.Unlock()

	return writeAuthConfigToFile(authConfigPath(boxID), cfg)
}

// GetAuthConfig vráti aktuálnu cached konfiguráciu. Bezpečné na concurrent
// call. Ak SyncAuthConfigFromEnv ešte nebehol, skúsi LoadAuthConfigFromFile
// najprv (re-hydratuje z disku po reštarte), inak vráti zero value.
func GetAuthConfig() AuthConfig {
	authConfigCache.mu.RLock()
	cfg := authConfigCache.cfg
	authConfigCache.mu.RUnlock()
	return cfg
}

// LoadAuthConfigFromFile načítá poslednú kešovanú konfiguráciu z disku.
// Použije sa ako fallback ak env premenné nie sú nastavené (napr. box bol
// spustený manuálne bez managera).
func LoadAuthConfigFromFile(boxID string) (AuthConfig, error) {
	var cfg AuthConfig
	data, err := os.ReadFile(authConfigPath(boxID))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	// Hydrate in-memory cache too — caller-side GetAuthConfig() dostane
	// rovnaké hodnoty bez ďalšieho file read.
	authConfigCache.mu.Lock()
	authConfigCache.cfg = cfg
	authConfigCache.mu.Unlock()
	return cfg, nil
}

// writeAuthConfigToFile zapíše config do JSON súboru, atomicky cez
// tmp + rename (zabráni partial write ak proces padne uprostred).
func writeAuthConfigToFile(path string, cfg AuthConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
