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
)

// APISelf Public Key - Used to verify license signatures
// Replace this with content from your generated public.pem
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

// ValidateLicense checks if the license key matches the signature using RSA
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
	// Create hash of the license data
	hashed := sha256.Sum256([]byte(licenseData))
	// Verify the signature
	err = rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashed[:], signature)
	if err != nil {
		return errors.New("invalid license signature")
	}
	return nil
}

// InitBox initializes the Box and checks for license in environment variables
func InitBox(conf BoxConfig) {
	fmt.Printf("🚀 APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)
	license := os.Getenv("APISELF_LICENSE")
	if license == "" {
		fmt.Println("❌ ERROR: APISELF_LICENSE environment variable is missing!")
		// In production, we exit to prevent unauthorized use
		// os.Exit(1)
	}
	fmt.Println("✅ License verification module initialized.")
}
