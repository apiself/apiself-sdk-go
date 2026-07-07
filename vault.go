package sdk

// Vault encrypts BYOK API keys at rest.
//
// Threat model: protect against DB/file leaks (laptop backup uploaded
// somewhere, box data dir emailed by mistake). Does NOT protect against an
// attacker with code execution on the same machine - they can derive the
// same key from the HWID. That stronger protection requires a user-supplied
// master password (lands with the manager-wide password feature).
//
// Construction:
//
//	salt = {BoxDataDir}/vault.salt   (32 random bytes, generated once)
//	hwid = GetHWID()                 (stable per-machine ID)
//	key  = Argon2id(hwid, salt, t=4, m=64MiB, p=4, len=32)
//	blob = nonce || AES-256-GCM-Seal(plaintext, nonce, aad=keyID)
//
// keyID as AAD binds a ciphertext to its row - moving a blob from one
// provider record to another fails authentication.
//
// This lives in the SDK (not per-box) so every AI box shares one hardened
// implementation instead of copying vault.go. See docs/box-ai-studio-spec.md.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
)

const (
	vaultSaltSize  = 32
	vaultKeySize   = 32 // AES-256
	vaultNonceSize = 12 // GCM standard
	// Argon2id parameters - calibrated for ~250ms on a modern laptop. Runs
	// once at box start, then the derived key is held in RAM for the
	// process lifetime.
	vaultArgonTime    = 4
	vaultArgonMemory  = 64 * 1024 // 64 MiB
	vaultArgonThreads = 4
)

// ErrVaultTampered is returned when AES-GCM authentication fails - either
// the ciphertext was modified or the AAD (keyID) doesn't match.
var ErrVaultTampered = errors.New("vault: ciphertext authentication failed")

// Vault holds the derived AES-GCM key in memory and exposes encrypt/decrypt
// scoped by an arbitrary key id (typically the provider id).
type Vault struct {
	gcm cipher.AEAD
}

// NewVault derives the vault key for this box. Generates the salt file on
// first call; subsequent calls re-derive the same key (assuming GetHWID()
// is stable, which is its contract).
func NewVault(boxID string) (*Vault, error) {
	sp, err := vaultSaltPath(boxID)
	if err != nil {
		return nil, err
	}
	salt, err := loadOrCreateVaultSalt(sp)
	if err != nil {
		return nil, fmt.Errorf("vault salt: %w", err)
	}
	hwid, err := GetHWID()
	if err != nil {
		return nil, fmt.Errorf("vault hwid: %w", err)
	}
	key := argon2.IDKey([]byte(hwid), salt, vaultArgonTime, vaultArgonMemory, vaultArgonThreads, vaultKeySize)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault gcm: %w", err)
	}
	return &Vault{gcm: gcm}, nil
}

// Encrypt wraps plaintext bound to keyID. Output = nonce || ciphertext+tag.
func (v *Vault) Encrypt(keyID string, plaintext string) ([]byte, error) {
	nonce := make([]byte, vaultNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return v.gcm.Seal(nonce, nonce, []byte(plaintext), []byte(keyID)), nil
}

// Decrypt unwraps a blob produced by Encrypt. Fails with ErrVaultTampered if
// the ciphertext was modified or keyID doesn't match the AAD used at seal.
func (v *Vault) Decrypt(keyID string, blob []byte) (string, error) {
	if len(blob) < vaultNonceSize+v.gcm.Overhead() {
		return "", ErrVaultTampered
	}
	nonce, ct := blob[:vaultNonceSize], blob[vaultNonceSize:]
	plain, err := v.gcm.Open(nil, nonce, ct, []byte(keyID))
	if err != nil {
		return "", ErrVaultTampered
	}
	return string(plain), nil
}

func vaultSaltPath(boxID string) (string, error) {
	dir := BoxDataDir(boxID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "vault.salt"), nil
}

func loadOrCreateVaultSalt(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		if len(data) != vaultSaltSize {
			return nil, fmt.Errorf("vault.salt corrupt: expected %d bytes, got %d", vaultSaltSize, len(data))
		}
		return data, nil
	}
	salt := make([]byte, vaultSaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, salt, 0o600); err != nil {
		return nil, err
	}
	return salt, nil
}
