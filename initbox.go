//go:build !screenshot

package sdk

import (
	"crypto/rsa"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// InitBox bootstraps the box: verifies the RS256 JWT licence, registers the
// instance with the cloud, starts background watchdogs. It returns usable
// LicenseClaims after a successful start. Strict licensing (since 2026-05):
// when APISELF_LICENSE is missing, the box exits — no graceful fallback to
// FREE mode without a JWT, because that allowed copied binaries to run any-
// where without HWID binding.
//
// Behaviour:
//
//   - APISELF_LICENSE missing       → exit 1 (strict; dev bypass via -tags dev)
//   - JWT signature invalid         → FREE mode (graceful, possible corruption)
//   - JWT expired (ExpiresAt past)  → FREE mode (selling point: never lose data)
//   - BoxID mismatch                → exit 1 (config bug, must be fixed)
//   - HWID mismatch                 → exit 1 (security boundary, copied JWT)
//   - max_instances limit reached   → FREE mode (degrade, don't exit)
//   - cloud-confirmed revocation    → runtime downgrade to FREE
//   - trial ExpiresAt reaches now   → runtime downgrade to FREE
//
// Dev bypass: builds compiled with `-tags dev` look at allowDevBypass = true
// (devtag_on.go); env var APISELF_DEV_LOCAL=1 then re-enables the legacy free
// fallback. Released binaries are compiled WITHOUT -tags dev → bypass code is
// dead-code at compile time, no env var can revive it.
//
// After return, boxes should gate features via sdk.HasTier("pro") rather than
// reading the returned LicenseClaims.Tier field — that field is the original
// claim and does not reflect runtime downgrades.
func InitBox(conf BoxConfig) LicenseClaims {
	myHWID, _ := GetHWID()

	fmt.Printf("APISelf: Starting %s (%s)...\n", conf.Name, conf.Version)

	// Auth box pubkey cache je per-box (na disku v BoxDataDir). Registruj
	// hostiteľský box ID hneď, aby ValidateJWT vedel kam cachovať pubkey.
	// V0.7+: GetUser používa lokálnu JWT validáciu v direct-port mode.
	SetAuthBoxID(conf.ID)

	licenseToken := os.Getenv("APISELF_LICENSE")
	if licenseToken == "" {
		// Dev bypass: only present in -tags dev builds. allowDevBypass je
		// false v released binárkach (devtag_off.go), takže celý tento
		// blok je dead-code mimo dev build-ov.
		if allowDevBypass && os.Getenv("APISELF_DEV_LOCAL") == "1" {
			fmt.Printf("APISelf: APISELF_LICENSE not set — DEV BYPASS active (compiled with -tags dev).\n  Hardware ID: %s\n", myHWID)
			setGlobalTier("free")
			return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
		}
		// Strict licensing: missing JWT znamená že box bol spustený mimo
		// Manager-a (typicky: skopírovaná binárka na inom stroji). Manager
		// generuje FREE JWT silently pri install / startup, takže legitimate
		// path nikdy nedosiahne túto vetvu.
		fmt.Fprintf(os.Stderr,
			"ERROR: APISELF_LICENSE not set.\n"+
				"  This box must be installed via APISelf Manager (free license\n"+
				"  is auto-generated at install). For air-gap / headless setups\n"+
				"  visit https://apiself.com/free-license to get a HWID-bound\n"+
				"  free license offline.\n\n"+
				"  Hardware ID: %s\n  Box ID:      %s\n",
			myHWID, conf.ID)
		os.Exit(1)
	}

	cloudPubKey, err := jwt.ParseRSAPublicKeyFromPEM([]byte(publicKeyPEM))
	if err != nil {
		fmt.Printf("APISelf: invalid embedded public key (%v) — running in FREE mode.\n", err)
		setGlobalTier("free")
		return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
	}

	// APISELF_MANAGER_PUBKEY env var je nastavený Manager-om (sdk-manager security
	// pass v0.21+). Slúži na validáciu lokálne-vystavených free JWT-ov keď cloud
	// nie je dostupný — Manager generuje keypair pri prvom štarte, sign-uje free
	// JWT-y s ním, SDK validuje s týmto pubkey-om iba ak claim `iss` ==
	// "apiself-manager-local". Pre paid tiery (basic/pro) sa stále vyžaduje
	// apiself.com cloud-signed JWT — Manager nemôže promote-núť tier.
	var managerPubKey *rsa.PublicKey
	if mgrPEM := os.Getenv("APISELF_MANAGER_PUBKEY"); mgrPEM != "" {
		if k, perr := jwt.ParseRSAPublicKeyFromPEM([]byte(mgrPEM)); perr == nil {
			managerPubKey = k
		}
	}

	claims := &LicenseClaims{}
	token, err := jwt.ParseWithClaims(licenseToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		// Vyber správny pubkey podľa `iss` claim-u.
		// "apiself-manager-local" → Manager pubkey (free tier only)
		// inak (vrátane prázdneho)  → cloud apiself.com pubkey
		if iss, _ := t.Claims.GetIssuer(); iss == "apiself-manager-local" {
			if managerPubKey == nil {
				return nil, fmt.Errorf("manager-issued JWT but APISELF_MANAGER_PUBKEY not set")
			}
			return managerPubKey, nil
		}
		return cloudPubKey, nil
	})
	if err != nil || !token.Valid {
		fmt.Printf("APISelf: licence invalid or expired (%v) — running in FREE mode.\n", err)
		setGlobalTier("free")
		return LicenseClaims{Plan: "free", Tier: "free", HWID: myHWID, BoxID: conf.ID}
	}

	// Security boundary: Manager-issued JWT-y MOZU byt iba free tier. Adversarial
	// Manager by inak mohol promote-núť ľubovoľný box na Pro/Enterprise bez platby.
	// Cloud-signed JWT-y (issuer = apiself.com alebo prázdne) môžu mať akýkoľvek tier.
	if iss, _ := token.Claims.GetIssuer(); iss == "apiself-manager-local" {
		if claims.Tier != "" && claims.Tier != "free" {
			fmt.Printf("ERROR: Manager-issued JWT cannot have tier=%q (only 'free' allowed). Refusing.\n", claims.Tier)
			os.Exit(1)
		}
		if claims.Plan != "" && claims.Plan != "free" {
			fmt.Printf("ERROR: Manager-issued JWT cannot have plan=%q (only 'free' allowed). Refusing.\n", claims.Plan)
			os.Exit(1)
		}
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
		// Trial JWT vystavený starou cloud verziou (alebo lokálnym manager-om)
		// nemá `tir` claim. Trial = grace period na vyskúšanie produktu —
		// dáme Pro tier počas trvania, trial expiry watchdog ho potom
		// downgraduje na free pri vyprseni `exp`. Bez tohto fix-u tier-gated
		// boxy (napr. apiself-box-storage) blokujú trial userov ako keby
		// boli na free pláne — confusing a podráždi prvých zákazníkov.
		if claims.Plan == "trial" {
			initialTier = "pro"
		} else {
			initialTier = "free"
		}
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
