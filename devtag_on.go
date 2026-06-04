//go:build dev

package sdk

// allowDevBypass je build-time konstanta. V dev build-och (`go build -tags dev`)
// je true -> env var APISELF_DEV_LOCAL=1 môže preskočiť strict licenčný check
// v initbox.go (užitočné pre lokálny vývoj boxov bez vystavovania JWT-u).
//
// Released binárky z marketplace túto cestu NEPOZNAJÚ - devtag_off.go nastavuje
// false a bypass kód sa stáva dead-code pri kompilácii.
const allowDevBypass = true
