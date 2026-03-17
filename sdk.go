package sdk

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/denisbrodbeck/machineid"
	"github.com/golang-jwt/jwt/v5"
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

// BoxConfig obsahuje identifikáciu boxu.
type BoxConfig struct {
	ID      string
	Name    string
	Version string
}

// LicenseClaims obsahuje RS256 JWT claims podľa APISelf v0.4.1.
type LicenseClaims struct {
	LicenseID string `json:"lid"` // UUID licencie
	BoxID     string `json:"bid"` // ID boxu (napr. apiself-box-helloworld)
	HWID         string `json:"hid"` // HWID stroja (machineid.ProtectedID)
	Email        string `json:"eml"` // Email majiteľa
	Plan         string `json:"pln"` // Plán: solo | team | unlimited
	MaxInstances int    `json:"mxi"` // Max súčasných inštancií (-1 = neobmedzene)
	jwt.RegisteredClaims
}

// GetHWID vráti stabilný HWID tohto stroja.
// Primárne používa machineid.ProtectedID("apiself") — HMAC-SHA256 machine-id,
// s fallback na SHA256(hostname:username) ak machine-id nie je dostupný.
func GetHWID() (string, error) {
	primary, err := machineid.ProtectedID("apiself")
	if err == nil {
		return primary, nil
	}
	hostname, _ := os.Hostname()
	u, _ := user.Current()
	h := sha256.Sum256([]byte(hostname + ":" + u.Username))
	log.Printf("APISelf: machine-id nedostupný, použitý HWID fallback")
	return hex.EncodeToString(h[:]), nil
}

// GetPort vráti port z APISELF_PORT env premennej alebo defaultPort ak nie je nastavená.
func GetPort(defaultPort int) int {
	if v := os.Getenv("APISELF_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			return p
		}
	}
	return defaultPort
}

// GetCoreURL vráti URL Core orchestrátora z APISELF_CORE_URL alebo default localhost:7474.
func GetCoreURL() string {
	if v := os.Getenv("APISELF_CORE_URL"); v != "" {
		return v
	}
	return "http://localhost:7474"
}

// InitBox overí RS256 JWT licenciu a zastaví box (os.Exit(1)) ak licencia nie je platná.
// Musí byť volaná ako prvá operácia v main() — pred otvorením akéhokoľvek portu.
// Vráti LicenseClaims pre použitie v boxe (email majiteľa, plán, atď.).
func InitBox(conf BoxConfig) LicenseClaims {
	myHWID, _ := GetHWID()

	fmt.Printf("APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)

	licenseToken := os.Getenv("APISELF_LICENSE")
	if licenseToken == "" {
		fmt.Printf("ERROR: APISELF_LICENSE chýba!\nHardware ID: %s\n", myHWID)
		os.Exit(1)
	}

	pubKey, err := jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyPEM))
	if err != nil {
		fmt.Printf("ERROR: neplatný verejný kľúč: %v\n", err)
		os.Exit(1)
	}

	claims := &LicenseClaims{}
	token, err := jwt.ParseWithClaims(licenseToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("neočakávaná podpisová metóda: %v", t.Header["alg"])
		}
		return pubKey, nil
	})
	if err != nil || !token.Valid {
		fmt.Printf("ERROR: neplatná licencia: %v\n", err)
		os.Exit(1)
	}

	if claims.BoxID != conf.ID {
		fmt.Printf("ERROR: Licencia je pre box '%s', nie '%s'\n", claims.BoxID, conf.ID)
		os.Exit(1)
	}

	if claims.HWID != myHWID {
		fmt.Printf("ERROR: Hardware mismatch!\n  Stroj:    %s\n  Licencia: %s\n", myHWID, claims.HWID)
		os.Exit(1)
	}

	fmt.Printf("OK: Licencia overená — %s (%s)\n", claims.Email, claims.Plan)

	cloudURL := os.Getenv("APISELF_CLOUD_URL")
	if cloudURL == "" {
		cloudURL = "https://apiself.com"
	}

	// ── Max instances enforcement ─────────────────────────────────────────────
	instanceID := newInstanceID()
	if claims.MaxInstances > 0 {
		if !registerInstance(instanceID, licenseToken, cloudURL) {
			fmt.Printf("APISelf FATAL: Dosiahnutý limit inštancií (%d). Box sa zastaví.\n", claims.MaxInstances)
			os.Exit(1)
		}
	} else {
		// Neobmedzený plán — len zaregistruj pre evidenciu (ignorujeme chyby)
		registerInstance(instanceID, licenseToken, cloudURL)
	}

	// ── Periodické kontroly na pozadí ────────────────────────────────────────
	go periodicRevocationCheck(conf, licenseToken, cloudURL)
	go instanceKeepAlive(instanceID, cloudURL)

	// ── Clean-up pri graceful shutdown ────────────────────────────────────────
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		unregisterInstance(instanceID, cloudURL)
		os.Exit(0)
	}()

	return *claims
}

// newInstanceID generuje náhodné UUID v4.
func newInstanceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant bits
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%12x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// registerInstance zaregistruje inštanciu na Cloud serveri.
// Vráti false ak limit bol dosiahnutý alebo server je dostupný a odmietol.
func registerInstance(instanceID, signedToken, cloudURL string) bool {
	body, _ := json.Marshal(map[string]string{
		"signedToken": signedToken,
		"instanceId":  instanceID,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(cloudURL+"/api/license/instance/register", "application/json", bytes.NewReader(body))
	if err != nil {
		// Cloud nedostupný — povolíme (offline tolerancia)
		log.Printf("APISelf: instance register zlyhal (cloud nedostupný): %v", err)
		return true
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
		Data    struct {
			Allowed bool `json:"allowed"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		// Neočakávaná odpoveď → offline tolerancia
		return true
	}
	return result.Data.Allowed
}

// instanceKeepAlive pinguje Cloud server každých 30 minút.
func instanceKeepAlive(instanceID, cloudURL string) {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		body, _ := json.Marshal(map[string]string{"instanceId": instanceID})
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(cloudURL+"/api/license/instance/ping", "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
		}
	}
}

// unregisterInstance odhlási inštanciu pri čistom ukončení.
func unregisterInstance(instanceID, cloudURL string) {
	body, _ := json.Marshal(map[string]string{"instanceId": instanceID})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(cloudURL+"/api/license/instance/unregister", "application/json", bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
}

// periodicRevocationCheck každých 6 hodín skontroluje platnosť licencie na Cloud serveri.
// Ak je licencia zrušená a Cloud dostupný → zastaví box (os.Exit(1)).
// Ak je Cloud nedostupný → preskočí (offline tolerancia).
func periodicRevocationCheck(conf BoxConfig, token, cloudURL string) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		if !isCloudOnline(cloudURL) {
			log.Printf("APISelf: Cloud nedostupný — revocation check preskočený")
			continue
		}

		body, _ := json.Marshal(map[string]string{"signedToken": token})
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(cloudURL+"/api/license/verify", "application/json", bytes.NewReader(body))
		if err != nil {
			continue
		}

		var result struct {
			Success bool `json:"success"`
			Data    struct {
				Valid bool `json:"valid"`
			} `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		if result.Success && !result.Data.Valid {
			fmt.Printf("APISelf FATAL: Licencia pre '%s' bola zrušená. Box sa zastaví.\n", conf.ID)
			os.Exit(1)
		}
	}
}

// isCloudOnline vráti true ak je Cloud API server dostupný.
func isCloudOnline(cloudURL string) bool {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cloudURL + "/api/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
