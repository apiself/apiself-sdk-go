package sdk

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// BoxConfigFile reprezentuje obsah `.apiself/config.json` (canonical source-of-truth
// pre box metadata: ID, name, version atd.).
//
// Pouzitie:
//
//	cfg, _ := sdk.LoadConfig()
//	license := sdk.InitBox(sdk.BoxConfig{
//	    ID:      cfg.ID,
//	    Name:    cfg.Name,
//	    Version: cfg.Version,
//	})
//
// Nahradzuje hardcoded `Version: "1.0.0"` v main.go ktore sa pri kazdom bumpe
// musi rucne menit. Po zmene config.json sa pri dalsom buil-de automaticky
// reflektne v `/api/info` aj v Manager UI.
type BoxConfigFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	Port        int    `json:"port,omitempty"`
}

// LoadConfig nacita `.apiself/config.json` zo standardnych cestiek (relativnych
// k binarke aj k cwd) a vrati parsed BoxConfigFile.
//
// Skusane cesty (v poradi):
//  1. ./.apiself/config.json (cwd)
//  2. ../.apiself/config.json (binarka v bin/)
//  3. {exe-dir}/.apiself/config.json
//  4. {exe-dir}/../.apiself/config.json
//
// Ak ziadny path nezbeha, vrati chybu. Box main.go by mal pouzit `Must` pattern:
//
//	cfg, err := sdk.LoadConfig()
//	if err != nil {
//	    log.Fatalf("config.json not found: %v", err)
//	}
func LoadConfig() (*BoxConfigFile, error) {
	candidates := []string{
		filepath.Join(".apiself", "config.json"),
		filepath.Join("..", ".apiself", "config.json"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, ".apiself", "config.json"),
			filepath.Join(dir, "..", ".apiself", "config.json"),
		)
	}

	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg BoxConfigFile
		if err := json.Unmarshal(data, &cfg); err != nil {
			continue
		}
		if cfg.ID == "" {
			continue
		}
		return &cfg, nil
	}
	return nil, errors.New("sdk.LoadConfig: .apiself/config.json not found in any candidate path")
}

// MustLoadConfig je convenience wrapper - panic-uje ak config nie je dostupny.
// Pouzivaj v main.go pri startup-e ked je config nutnost.
func MustLoadConfig() *BoxConfigFile {
	cfg, err := LoadConfig()
	if err != nil {
		panic(err)
	}
	return cfg
}
