//go:build !screenshot

package sdk

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// InitBox bootstraps the box: verifies the RS256 JWT licence, registers the
// instance with the cloud, starts background watchdogs. It always returns
// usable LicenseClaims — even when no valid licence is present, in which case
// the box runs in FREE tier mode (graceful fallback, no os.Exit).
//
// Behaviour:
//
//   - APISELF_LICENSE missing       → FREE mode (warn + return free claims)
//   - JWT signature invalid         → FREE mode
//   - JWT expired (ExpiresAt past)  → FREE mode
//   - BoxID mismatch                → exit 1 (config bug, must be fixed)
//   - HWID mismatch                 → exit 1 (security boundary)
//   - max_instances limit reached   → FREE mode (degrade, don't exit)
//   - cloud-confirmed revocation    → runtime downgrade to FREE
//   - trial ExpiresAt reaches now   → runtime downgrade to FREE
//
// After return, boxes should gate features via sdk.HasTier("pro") rather than
// reading the returned LicenseClaims.Tier field — that field is the original
// claim and does not reflect runtime downgrades.
func InitBox(conf BoxConfig) LicenseClaims {
	myHWID, _ := GetHWID()

	fmt.Printf("APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)

	licenseToken := os.Getenv("APISELF_LICENSE")
	if licenseToken == "" {
		fmt.Printf("APISelf: APISELF_LICENSE not set — running in FREE mode.\n  Hardware ID: %s\n", myHWID)
		setGlobalTier("free")
		return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
	}

	pubKey, err := jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyPEM))
	if err != nil {
		fmt.Printf("APISelf: invalid embedded public key (%v) — running in FREE mode.\n", err)
		setGlobalTier("free")
		return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
	}

	claims := &LicenseClaims{}
	token, err := jwt.ParseWithClaims(licenseToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return pubKey, nil
	})
	if err != nil || !token.Valid {
		fmt.Printf("APISelf: licence invalid or expired (%v) — running in FREE mode.\n", err)
		setGlobalTier("free")
		return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
	}

	if claims.BoxID != conf.ID {
		// Config error: the licence is for a different box. This is a deployment
		// mistake (operator gave the wrong APISELF_LICENSE), not a malicious
		// scenario. Exit so the operator notices immediately.
		fmt.Printf("ERROR: licence is for box '%s', not '%s'\n", claims.BoxID, conf.ID)
		os.Exit(1)
	}

	if claims.HWID != myHWID {
		// Security boundary: a licence file copied to another machine. We cannot
		// graceful-degrade here — that would let stolen licence files run any-
		// where in FREE mode AND keep the supervisor from noticing.
		fmt.Printf("ERROR: hardware mismatch!\n  Machine: %s\n  Licence: %s\n", myHWID, claims.HWID)
		os.Exit(1)
	}

	fmt.Printf("OK: licence verified — %s (plan=%s, tier=%s)\n", claims.Email, claims.Plan, claims.Tier)

	initialTier := claims.Tier
	if initialTier == "" {
		// Legacy free / trial tokens with no tir claim — treat as free.
		initialTier = "free"
	}
	state := setGlobalTier(initialTier)

	cloudURL := os.Getenv("APISELF_CLOUD_URL")
	if cloudURL == "" {
		cloudURL = "https://apiself.com"
	}

	instanceID := newInstanceID()
	if claims.MaxInstances > 0 {
		if !registerInstance(instanceID, licenseToken, cloudURL) {
			fmt.Printf("APISelf: instance limit (%d) reached — running in FREE mode.\n", claims.MaxInstances)
			state.downgradeToFree()
		}
	} else {
		registerInstance(instanceID, licenseToken, cloudURL)
	}

	go periodicRevocationCheck(conf, licenseToken, cloudURL, state)
	go trialExpiryWatchdog(conf, claims, state)
	go instanceKeepAlive(instanceID, cloudURL)

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		unregisterInstance(instanceID, cloudURL)
		os.Exit(0)
	}()

	return *claims
}

// trialExpiryWatchdog sleeps until ExpiresAt and then downgrades the runtime
// tier to FREE. If the JWT has no ExpiresAt (perpetual licence) the goroutine
// exits immediately. Triggered downgrade is logged so it appears in box logs
// next to revocation events.
func trialExpiryWatchdog(conf BoxConfig, claims *LicenseClaims, state *licenseState) {
	if claims.ExpiresAt == nil {
		return
	}
	expiry := claims.ExpiresAt.Time
	now := time.Now()
	if !expiry.After(now) {
		// Should have been caught by JWT validation, but be defensive.
		state.downgradeToFree()
		return
	}
	timer := time.NewTimer(expiry.Sub(now))
	defer timer.Stop()
	<-timer.C
	fmt.Printf("APISelf: licence/trial for '%s' expired — continuing in FREE mode.\n", conf.ID)
	state.downgradeToFree()
}
