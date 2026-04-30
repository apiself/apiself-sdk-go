//go:build screenshot

package sdk

import "fmt"

// InitBox v screenshot builde preskočí overenie licencie.
// Tento súbor je skompilovaný LEN s: go build -tags screenshot
// Distribuované binárky NIKDY neobsahujú tento kód — kompilátor ho vystrihne.
//
// Screenshot mode pretends to hold the highest tier (Pro) so screenshot
// rendering can exercise every code path including premium-gated features.
func InitBox(conf BoxConfig) LicenseClaims {
	myHWID, _ := GetHWID()
	fmt.Printf("APISelf: Screenshot mód — %s (%s), licencia preskočená.\n", conf.Name, conf.Version)
	setGlobalTier("pro")
	return LicenseClaims{BoxID: conf.ID, HWID: myHWID, Plan: "screenshot", Tier: "pro"}
}
