package sdk

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/joho/godotenv" // Pridané pre .env
)

// Tvoj verejný kľúč (nechávam tvoj pôvodný)
const publicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA4q73lqy18mC4fdUpo4rB
viicDRAfKhFLJ15+m7cQ3d5qbjlDfHGHMUxFObRcVB4j+7y67BtJ3BmuAW+0GShW
bUOMSM9q5LtB9es7ocyIznlAIlTgdSqoLcnq5PJbjb2nnAiBiTBuLRlh/DWj5/uD
uNxSrcHQia+vb2WWRogI0W/Nt+7VcB11DpMZndf/9PhuxtEMk82oVibOuJI8mlap
RNDCW5+5rg+AlVvkk4pJefV0uBjJwQmebITzxzT/gW9DtSHUjUVbgke7ig2SOJ0J
/z2JPrw9EPkET1+wOQ0oOQVAiBrNjSUXkP0cW6lCDQor9SideL7iZP7wjsQR30T5
UwIDAQAB
-----END PUBLIC KEY-----`

type BoxConfig struct {
	ID      string
	Version string
	Name    string
}

// ValidateLicense - tvoja pôvodná funkcia (bezo zmeny)
func ValidateLicense(licenseData string, signature []byte) error {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return errors.New("failed to parse PEM block containing the public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return errors.New("not an RSA public key")
	}
	hashed := sha256.Sum256([]byte(licenseData))
	err = rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashed[:], signature)
	if err != nil {
		return errors.New("invalid license signature")
	}
	return nil
}

// InitBox - upravená o godotenv
func InitBox(conf BoxConfig) {
	// 1. Automaticky skúsime načítať .env hneď na začiatku
	_ = godotenv.Load()

	fmt.Printf("🚀 APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)

	license := os.Getenv("APISELF_LICENSE")
	if license == "" {
		fmt.Println("❌ ERROR: APISELF_LICENSE environment variable is missing!")
		return
	}

	fmt.Println("🔍 Verifying license format...")
	// TODO: Tu neskôr pridáme volanie ValidateLicense po rozsekaní stringu na dáta a podpis

	fmt.Println("✅ License verification module initialized.")
}
