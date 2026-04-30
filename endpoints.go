package sdk

import (
	"encoding/json"
	"net/http"
	"runtime"
	"time"
)

// BoxInfo je metadata payload, ktorý box poskytuje na `/api/info`.
// Manager / marketplace / monitoring tooling ho používa na zobrazenie verzie,
// plánu, tieru a zoznamu endpointov.
//
// Polia sú core fields — `Custom` mapa umožňuje box-špecifické rozšírenia
// (napr. filedrop môže vrátiť `{"alias":"jarik"}`, recorder `{"format":"mp4"}`).
type BoxInfo struct {
	ID            string                 `json:"id"`
	Name          string                 `json:"name"`
	Version       string                 `json:"version"`
	Author        string                 `json:"author,omitempty"`        // z config.json
	Description   string                 `json:"description,omitempty"`
	Plan          string                 `json:"plan,omitempty"`          // pôvodný `pln` claim z JWT (free|trial|basic|pro)
	Tier          string                 `json:"tier,omitempty"`          // CURRENT effective tier (free|basic|pro). Reflektuje runtime downgrade.
	OriginalTier  string                 `json:"originalTier,omitempty"`  // tier zo zakúpeného JWT — odlíši sa od `Tier` po expirácii
	LicenseExpired bool                  `json:"licenseExpired,omitempty"` // true ak SDK downgradoval na free
	Email         string                 `json:"email,omitempty"`         // owner email z license
	HWID          string                 `json:"hwid,omitempty"`          // stroj na ktorom box beží
	Endpoints     []string               `json:"endpoints,omitempty"`     // ["GET /api/health", "POST /api/links", ...]
	Custom        map[string]interface{} `json:"custom,omitempty"`        // box-špecifické extra polia
}

// HealthResponse je payload na `/api/health` — minimálny liveness probe.
// Manager supervisor môže túto odpoveď použiť pre health-check polling.
type HealthResponse struct {
	Status   string  `json:"status"`             // vždy "ok" pri 200
	Uptime   float64 `json:"uptime"`             // sekúnd od štartu boxu
	Version  string  `json:"version"`
	GoMemMB  uint64  `json:"goMemMb,omitempty"`  // alloc memory v MB (diagnostika)
}

// RegisterRequiredEndpoints zaregistruje povinné `/api/health` a `/api/info`
// endpointy na danom mux-e. Každý APISelf box musí tieto endpointy mať —
// volaním tejto funkcie boxy získajú konzistentnú implementáciu zadarmo.
//
// `infoFn` je closure ktorá vráti aktuálne BoxInfo — closure preto, aby box
// mohol meniť mutable polia (napr. počet aktívnych spojení v Custom mape) bez
// re-registrácie handlerov.
//
// Použitie:
//
//	mux := http.NewServeMux()
//	startedAt := time.Now()
//	sdk.RegisterRequiredEndpoints(mux, func() sdk.BoxInfo {
//	    return sdk.BoxInfo{
//	        ID:      "apiself-box-helloworld",
//	        Name:    "Hello World",
//	        Version: "1.0.0",
//	        Plan:    license.Plan,
//	        Tier:    license.Tier,
//	    }
//	})
//	mux.HandleFunc("/api/hello", h.Hello)
//
// Endpointy sú **bez autentifikácie** — slúžia ako liveness probe pre
// orchestrátor a discovery pre manager UI. Žiadne citlivé dáta v info payload-e.
func RegisterRequiredEndpoints(mux *http.ServeMux, infoFn func() BoxInfo) {
	startedAt := time.Now()

	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		writeAPI(w, http.StatusOK, HealthResponse{
			Status:  "ok",
			Uptime:  time.Since(startedAt).Seconds(),
			Version: infoFn().Version,
			GoMemMB: ms.Alloc / 1024 / 1024,
		})
	})

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		info := infoFn()
		// Overlay runtime tier state so /api/info reflects post-downgrade reality
		// (e.g. trial expired → Tier="free", OriginalTier="pro", LicenseExpired=true).
		// Boxes can still set Tier themselves; if they do, OriginalTier preserves
		// what they passed in — useful for showing "you were on Pro, now on Free".
		runtime := Tier()
		if info.Tier != "" && info.Tier != runtime {
			info.OriginalTier = info.Tier
		}
		info.Tier = runtime
		info.LicenseExpired = IsLicenseExpired()
		writeAPI(w, http.StatusOK, info)
	})
}

// writeAPI je miniwrap pre konzistentný `{success, data}` envelope
// (rovnaký pattern ako apiself-com a apiself-manager používajú).
func writeAPI(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"data":    data,
	})
}
