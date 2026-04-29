package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EndpointsFromSwagger reads `.apiself/swagger.json` and returns its paths
// formatted as ["METHOD /path", ...] sorted by path then method. This is the
// canonical source of truth for the `BoxInfo.Endpoints` field — boxes should
// not maintain a separate hand-written slice that drifts from swagger.
//
// Same candidate paths as LoadConfig (cwd, parent, exe-dir). Returns nil if
// swagger.json is missing or unparseable; the box still serves /api/info but
// the endpoints field will be omitted.
//
// Usage in box main.go:
//
//	sdk.RegisterRequiredEndpoints(mux, func() sdk.BoxInfo {
//	    return sdk.BoxInfo{
//	        ID:        cfg.ID,
//	        Version:   cfg.Version,
//	        Endpoints: sdk.EndpointsFromSwagger(),
//	    }
//	})
func EndpointsFromSwagger() []string {
	candidates := []string{
		filepath.Join(".apiself", "swagger.json"),
		filepath.Join("..", ".apiself", "swagger.json"),
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, ".apiself", "swagger.json"),
			filepath.Join(dir, "..", ".apiself", "swagger.json"),
		)
	}

	var data []byte
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			data = b
			break
		}
	}
	if data == nil {
		return nil
	}

	var doc struct {
		Paths map[string]map[string]interface{} `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	// HTTP method whitelist — swagger paths can include vendor extensions
	// (x-foo) and shared parameter sections we don't want to render as endpoints.
	allowed := map[string]struct{}{
		"get": {}, "post": {}, "put": {}, "delete": {},
		"patch": {}, "head": {}, "options": {},
	}

	out := make([]string, 0, len(doc.Paths)*2)
	for path, methods := range doc.Paths {
		for method := range methods {
			if _, ok := allowed[strings.ToLower(method)]; !ok {
				continue
			}
			out = append(out, strings.ToUpper(method)+" "+path)
		}
	}
	sort.Strings(out)
	return out
}
