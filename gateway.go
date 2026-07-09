package sdk

// Consumer-side helpers for the AI Gateway box (apiself-box-ai-gateway).
//
// A specialist AI box (image-gen, tts, …) can route its CLOUD calls through
// the Gateway so the provider key lives in ONE place (the Gateway's vault)
// instead of being re-entered per box. These helpers are best-effort: if the
// Gateway isn't installed / running / configured, they return an error and
// the caller falls back to its own local provider store. Non-breaking.
//
// Spec: docs/box-ai-gateway-spec.md §7 (resolve + route).

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// GatewayBoxID is the canonical id of the AI Gateway box.
const GatewayBoxID = "apiself-box-ai-gateway"

// GatewayAvailable reports whether the AI Gateway box is installed + running.
// Cheap manager-local lookup; use it to decide whether to prefer the Gateway
// over a box-local key.
func GatewayAvailable(ctx context.Context) bool {
	a, err := IsBoxAvailable(ctx, GatewayBoxID, "", 2*time.Second)
	return err == nil && a != nil && a.Reason == "ok"
}

// GatewayImage routes an image generation to the Gateway, which resolves the
// provider from the model id, loads the key from its vault, and calls the
// adapter. Returns the PNG bytes. Error when the Gateway is unavailable, has
// no key for the model's provider, or the provider call fails — the caller
// should then fall back to its own path.
func GatewayImage(ctx context.Context, model, prompt, size string) ([]byte, error) {
	payload, _ := json.Marshal(map[string]any{
		"capability": "image",
		"model":      model,
		"request":    map[string]any{"prompt": prompt, "size": size},
	})
	resp, err := CallBox(ctx, GatewayBoxID, http.MethodPost, "/api/ai-gateway/route",
		bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var env struct {
		Success bool `json:"success"`
		Data    struct {
			DataB64 string `json:"data_b64"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("gateway route decode: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("gateway route: %s", env.Error)
	}
	data, err := base64.StdEncoding.DecodeString(env.Data.DataB64)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("gateway route: empty image")
	}
	return data, nil
}
