package sdk

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PlatformDataDir vráti koreňový APISelf data adresár podľa OS konvencií.
//
//	Windows: %LOCALAPPDATA%\APISelf
//	macOS:   ~/Library/Application Support/APISelf
//	Linux:   $XDG_DATA_HOME/apiself  alebo  ~/.local/share/apiself
//
// Env override APISELF_DATA_DIR prepíše celý root.
func PlatformDataDir() string {
	if env := os.Getenv("APISELF_DATA_DIR"); env != "" {
		return env
	}
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "APISelf")
		}
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "APISelf")
	default: // linux a ostatné unixy
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return filepath.Join(d, "apiself")
		}
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".local", "share", "apiself")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".apiself")
}

// BoxDataDir vráti štandardné úložisko pre konkrétny box:
//
//	{PlatformDataDir}/boxes/{boxID}
//
// Manager injektuje APISELF_BOX_DATA_DIR keď box spúšťa sám; táto premenná má
// prioritu. Ak nie je, box bežiaci priamo (dev, make run) použije platformovú
// cestu a automaticky migruje staré dáta zo `~/.apiself/{suffix}` kde
// legacySuffixes sú historické názvy adresárov ktoré box predtým používal
// (napr. "filedrop" pre apiself-box-filedrop).
//
// Príklad:
//
//	dir := sdk.BoxDataDir("apiself-box-filedrop", "filedrop")
//	// Windows:  C:\Users\X\AppData\Local\APISelf\boxes\apiself-box-filedrop
//	// macOS:    ~/Library/Application Support/APISelf/boxes/apiself-box-filedrop
//	// Linux:    ~/.local/share/apiself/boxes/apiself-box-filedrop
//
// Návratová cesta je garantovane vytvorená (MkdirAll).
func BoxDataDir(boxID string, legacySuffixes ...string) string {
	if env := os.Getenv("APISELF_BOX_DATA_DIR"); env != "" {
		_ = os.MkdirAll(env, 0o750)
		return env
	}
	target := filepath.Join(PlatformDataDir(), "boxes", boxID)

	// Migruj zo staršieho umiestnenia, ak ešte nový neexistuje.
	if _, err := os.Stat(target); os.IsNotExist(err) {
		if home, err := os.UserHomeDir(); err == nil {
			for _, suffix := range legacySuffixes {
				if suffix == "" {
					continue
				}
				legacy := filepath.Join(home, ".apiself", suffix)
				if st, err := os.Stat(legacy); err == nil && st.IsDir() {
					if err := os.MkdirAll(filepath.Dir(target), 0o750); err == nil {
						if renameErr := os.Rename(legacy, target); renameErr == nil {
							break
						}
					}
				}
			}
		}
	}

	_ = os.MkdirAll(target, 0o750)
	return target
}

// BoxDBDir returns the canonical SQLite directory for a box:
//
//	{BoxDataDir(boxID)}/db
//
// The directory is created if absent. On first call any legacy `*.db`
// files (and their `-shm` / `-wal` SQLite WAL sidecars) found at the root
// of BoxDataDir are auto-migrated into `db/` so existing installations
// upgrade transparently. Migration uses os.Rename (atomic on the same
// filesystem) and is idempotent — once moved, the root has no .db files
// so subsequent calls do nothing.
//
// Use it together with sdk.BoxDataDir():
//
//	boxDataDir := sdk.BoxDataDir(cfg.ID, "filedrop")
//	dbDir, _   := sdk.BoxDBDir(cfg.ID)
//	db.Open(filepath.Join(dbDir, "filedrop.db"))
func BoxDBDir(boxID string) (string, error) {
	root := BoxDataDir(boxID)
	dbDir := filepath.Join(root, "db")
	if err := os.MkdirAll(dbDir, 0o750); err != nil {
		return "", err
	}
	migrateLegacyDBFiles(root, dbDir)
	return dbDir, nil
}

// BoxDBPath is BoxDBDir + a filename. Convenience wrapper for the common
// "open one well-known SQLite file" use case.
//
//	p, _ := sdk.BoxDBPath(cfg.ID, "filedrop.db")
//	db.Open(p)
func BoxDBPath(boxID, name string) (string, error) {
	dir, err := BoxDBDir(boxID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// migrateLegacyDBFiles moves *.db / *.db-shm / *.db-wal files from `root`
// into `dbDir`. Errors are swallowed — a partial migration is recoverable on
// the next call, and noisy logs at startup would scare users without
// providing a useful path forward (we don't have a logger wired up yet at
// this layer).
func migrateLegacyDBFiles(root, dbDir string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// Match foo.db, foo.db-shm, foo.db-wal — but NOT files that just
		// happen to contain ".db" (e.g. "config.dbg.json"). Anchored on
		// either ".db" tail or ".db-" infix.
		if !(strings.HasSuffix(n, ".db") ||
			strings.Contains(n, ".db-shm") ||
			strings.Contains(n, ".db-wal")) {
			continue
		}
		src := filepath.Join(root, n)
		dst := filepath.Join(dbDir, n)
		// Don't overwrite — if the new path already has a file, the old root
		// copy is stale and should be removed manually by the operator.
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		_ = os.Rename(src, dst)
	}
}
