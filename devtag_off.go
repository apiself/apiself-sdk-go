//go:build !dev

package sdk

// allowDevBypass je build-time konstanta. Default = false (release builds).
// Production binárky kompilované bez `-tags dev` majú allowDevBypass = false;
// bypass kód v initbox.go sa stáva dead-code-em pri kompilácii.
//
// Pre dev workflow: `go build -tags dev` → načíta sa devtag_on.go kde je
// allowDevBypass = true a env var APISELF_DEV_LOCAL=1 môže preskočiť strict
// licenčný check.
//
// Bezpečnostný model: aj keby útočník vedel meno tagu a env var, potrebuje
// source kód + Go kompilátor pre vlastnú binárku. Released binárky z
// marketplace bypass NEMAJÚ.
const allowDevBypass = false
