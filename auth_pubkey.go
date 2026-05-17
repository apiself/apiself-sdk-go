package sdk

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Auth pubkey distribution & local JWT validation (v0.7).
//
// Background: pred v0.7 manager validoval auth-box-issued JWT sám a injektoval
// X-APISelf-* hlavičky do proxy requestu na box; box im veril bez vlastnej
// validácie. Nevýhody:
//
//  1. Direct-port access bol tvrdo blokovaný (RequireManagerProxy → 401).
//  2. Ak manager spadol, box nemal ako overiť cookie sám.
//  3. Ak auth box spadol, UI degradovalo na placeholder owner (security bypass).
//
// V0.7 box pri prvom štarte fetchne auth-box pubkey + uloží na disk
// ({BoxDataDir}/auth_pubkey.pem). Pri validácii cookie / Bearer tokenu
// si box vystačí lokálne — žiadny network hop, žiadna závislosť od managera.

// authPubKeyState drží lazy-loaded RSA public key auth boxu + súbor s
// jeho PEM kópiou. Cached medzi requestami; refresh keď validácia zlyhá.
type authPubKeyState struct {
	mu     sync.RWMutex
	parsed *rsa.PublicKey
	pem    string // raw PEM, what was on disk / returned by /api/auth/pubkey
}

var globalAuthPubKey = &authPubKeyState{}

// authPubKeyPath vráti cestu k cache súboru pre daný box ID.
// Box ID je box ktorý SDK volá z (forms, recorder, …), NIE auth box.
func authPubKeyPath(boxID string) string {
	return filepath.Join(BoxDataDir(boxID), "auth_pubkey.pem")
}

// LoadAuthPubKey lazy-loaduje auth-box pubkey buď z disku, alebo
// (ak chýba / je starý) fetchne z auth boxu cez APISELF_AUTH_BOX_URL.
//
// boxID je hosting box (ten ktorý SDK kompilujeme do — forms, recorder, …),
// nie auth box. Cesta cache: {BoxDataDir(boxID)}/auth_pubkey.pem.
//
// Vracia (nil, nil) ak APISELF_AUTH_BOX_URL nie je nastavené — box beží
// v single-user móde a tento helper sa nemal volať. Caller (GetUser) v tom
// prípade pokračuje bez auth validácie.
//
// Bezpečné na concurrent volanie — sync.RWMutex interne.
func LoadAuthPubKey(boxID string) (*rsa.PublicKey, error) {
	globalAuthPubKey.mu.RLock()
	if globalAuthPubKey.parsed != nil {
		defer globalAuthPubKey.mu.RUnlock()
		return globalAuthPubKey.parsed, nil
	}
	globalAuthPubKey.mu.RUnlock()

	globalAuthPubKey.mu.Lock()
	defer globalAuthPubKey.mu.Unlock()
	// Re-check po acquire write-lock
	if globalAuthPubKey.parsed != nil {
		return globalAuthPubKey.parsed, nil
	}

	// Cesta 1 — disk cache
	path := authPubKeyPath(boxID)
	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		if pub, perr := parsePubKeyPEM(string(data)); perr == nil {
			globalAuthPubKey.parsed = pub
			globalAuthPubKey.pem = string(data)
			return pub, nil
		}
		// Súbor je rozbitý — pokračuj na fetch
	}

	// Cesta 2 — fetch z auth boxu
	url := os.Getenv("APISELF_AUTH_BOX_URL")
	if url == "" {
		return nil, errors.New("APISELF_AUTH_BOX_URL not set — auth box not detected")
	}
	pemStr, err := fetchAuthPubKey(url)
	if err != nil {
		return nil, fmt.Errorf("fetch auth pubkey: %w", err)
	}
	pub, err := parsePubKeyPEM(pemStr)
	if err != nil {
		return nil, fmt.Errorf("parse fetched pubkey: %w", err)
	}
	// Ulož na disk — best-effort, nezlyháme ak write zlyhá
	_ = os.WriteFile(path, []byte(pemStr), 0o600)
	globalAuthPubKey.parsed = pub
	globalAuthPubKey.pem = pemStr
	return pub, nil
}

// RefreshAuthPubKey nuluje cache a vynúti opakovaný fetch z auth boxu.
// Volaj keď JWT validácia zlyhá so signature error — možná pubkey rotácia.
func RefreshAuthPubKey(boxID string) error {
	globalAuthPubKey.mu.Lock()
	globalAuthPubKey.parsed = nil
	globalAuthPubKey.pem = ""
	globalAuthPubKey.mu.Unlock()
	_, err := LoadAuthPubKey(boxID)
	return err
}

func parsePubKeyPEM(pemStr string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("invalid PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not RSA public key")
	}
	return rsaKey, nil
}

// fetchAuthPubKey volá auth-box /api/auth/pubkey s 5s timeout-om.
// Vracia raw PEM string z `data.publicKeyPEM` poľa.
func fetchAuthPubKey(baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/auth/pubkey", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return "", err
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			PublicKeyPEM string `json:"publicKeyPEM"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", err
	}
	if !env.Success || env.Data.PublicKeyPEM == "" {
		return "", errors.New("auth box returned empty pubkey")
	}
	return env.Data.PublicKeyPEM, nil
}
