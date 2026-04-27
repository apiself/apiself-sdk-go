//go:build !screenshot

package sdk

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/golang-jwt/jwt/v5"
)

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
