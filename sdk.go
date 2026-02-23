package sdk

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/denisbrodbeck/machineid"
	"github.com/joho/godotenv"
)

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

// LicensePayload obsahuje dáta zakódované v licencii
type LicensePayload struct {
	BoxID     string   `json:"bid"` // Box ID
	ClusterID string   `json:"cid"` // ID Grupy (pre zdieľané licencie)
	HWID      string   `json:"hid"` // Uzamknutie na konkrétny HW
	ValidHW   []string `json:"vhw"` // Zoznam povolených HW v rámci Grupy
	Owner     string   `json:"own"` // Meno zákazníka
}

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
	return rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, hashed[:], signature)
}

func InitBox(conf BoxConfig) {
	_ = godotenv.Load()

	// 1. Získame unikátny HWID tohto stroja
	myHWID, _ := machineid.ID()

	fmt.Printf("🚀 APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)

	license := os.Getenv("APISELF_LICENSE")
	if license == "" {
		fmt.Printf("❌ ERROR: APISELF_LICENSE missing!\n🔑 Your Hardware ID: %s\n", myHWID)
		os.Exit(1) // Tu už dávame os.Exit, aby sa port neotvoril
	}

	// 2. Rozdelenie na Dáta a Podpis (formát: base64JSON.base64Sig)
	parts := strings.Split(license, ".")
	if len(parts) != 2 {
		fmt.Println("❌ ERROR: Invalid license format!")
		os.Exit(1)
	}
	// 3. Verifikácia RSA podpisu
	decodedData, _ := base64.StdEncoding.DecodeString(parts[0])
	signature, _ := base64.StdEncoding.DecodeString(parts[1])
	if err := ValidateLicense(string(decodedData), signature); err != nil {
		fmt.Printf("❌ ERROR: %v\n", err)
		os.Exit(1)
	}
	// 4. Kontrola obsahu licencie (JSON)
	var payload LicensePayload
	if err := json.Unmarshal(decodedData, &payload); err != nil {
		fmt.Println("❌ ERROR: Failed to parse license data!")
		os.Exit(1)
	}
	// 5. NODE-LOCK & CLUSTER-CHECK
	// Kontrolujeme, či môj HWID sedí s tým v licencii, alebo či som v zozname Grupy
	authorized := (payload.HWID == myHWID)
	// Ak je to skupinová licencia, skontrolujeme zoznam povolených HW
	if !authorized && len(payload.ValidHW) > 0 {
		for _, v := range payload.ValidHW {
			if v == myHWID {
				authorized = true
				break
			}
		}
	}
	if !authorized {
		fmt.Printf("❌ ERROR: Hardware mismatch!\n   Machine ID: %s\n   License ID: %s\n", myHWID, payload.HWID)
		os.Exit(1)
	}
	// 6. Kontrola Box ID
	if payload.BoxID != conf.ID {
		fmt.Printf("❌ ERROR: License is for Box '%s', but you are running '%s'\n", payload.BoxID, conf.ID)
		os.Exit(1)
	}
	fmt.Printf("✅ License verified for: %s (Cluster: %s)\n", payload.Owner, payload.ClusterID)
}
