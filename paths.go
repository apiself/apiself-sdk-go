package sdk

import (
	"os"
	"path/filepath"
	"runtime"
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
