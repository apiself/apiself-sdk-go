//go:build screenshot

package sdk

import "fmt"

// InitBox v screenshot builde preskočí overenie licencie.
// Tento súbor je skompilovaný LEN s: go build -tags screenshot
// Distribuované binárky NIKDY neobsahujú tento kód — kompilátor ho vystrihne.
func InitBox(conf BoxConfig) LicenseClaims {
	myHWID, _ := GetHWID()
	fmt.Printf("APISelf: Screenshot mód — %s (%s), licencia preskočená.\n", conf.Name, conf.Version)
	return LicenseClaims{BoxID: conf.ID, HWID: myHWID, Plan: "screenshot"}
}
