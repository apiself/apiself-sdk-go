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
	"net/url"
	"time"
)

// GatewayBoxID is the canonical id of the AI Gateway box.
const GatewayBoxID = "apiself-box-ai-gateway"

// GatewayResolution is how a model should be run, from the Gateway's resolve
// endpoint. Mode is "local" (specialist runs its own engine on FilePath) or
// "cloud" (call the Gateway route; Provider holds the key, never exposed).
type GatewayResolution struct {
	Mode       string            `json:"mode"`
	Model      string            `json:"model"`
	FilePath   string            `json:"filePath"`
	Kind       string            `json:"kind"`
	Companions map[string]string `json:"companions"`
	Provider   string            `json:"provider"`
}

// GatewayResolve asks the Gateway how to run a model of the given capability.
// Used by a specialist box's backend: local → run own engine with the
// returned path/kind/companions; cloud → call GatewayImage (or route).
func GatewayResolve(ctx context.Context, capability, model string) (*GatewayResolution, error) {
	path := "/api/ai-gateway/resolve?capability=" + url.QueryEscape(capability) + "&model=" + url.QueryEscape(model)
	resp, err := CallBox(ctx, GatewayBoxID, http.MethodGet, path, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool              `json:"success"`
		Data    GatewayResolution `json:"data"`
		Error   string            `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("gateway resolve decode: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("gateway resolve: %s", env.Error)
	}
	return &env.Data, nil
}

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

// GatewayVideo routes a text-to-video generation through the Gateway. Returns
// the raw video bytes + its content type (usually "video/mp4"). Blocking:
// video generation runs tens of seconds to minutes (the Gateway submits, polls
// the provider, downloads the result). Same fallback semantics as GatewayImage.
func GatewayVideo(ctx context.Context, model, prompt, aspectRatio string, durationSec int, resolution string) ([]byte, string, error) {
	payload, _ := json.Marshal(map[string]any{
		"capability": "video",
		"model":      model,
		"request": map[string]any{
			"prompt": prompt, "aspect_ratio": aspectRatio,
			"duration": durationSec, "resolution": resolution,
		},
	})
	resp, err := CallBox(ctx, GatewayBoxID, http.MethodPost, "/api/ai-gateway/route",
		bytes.NewReader(payload), "application/json")
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			DataB64  string `json:"data_b64"`
			MimeType string `json:"mime_type"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, "", fmt.Errorf("gateway route decode: %w", err)
	}
	if !env.Success {
		return nil, "", fmt.Errorf("gateway route: %s", env.Error)
	}
	data, err := base64.StdEncoding.DecodeString(env.Data.DataB64)
	if err != nil || len(data) == 0 {
		return nil, "", fmt.Errorf("gateway route: empty video")
	}
	ct := env.Data.MimeType
	if ct == "" {
		ct = "video/mp4"
	}
	return data, ct, nil
}

// GatewayTranscribe routes a speech-to-text request through the Gateway. The
// audio bytes are base64-encoded in the payload; returns the transcript text.
// Same fallback semantics as GatewayImage.
func GatewayTranscribe(ctx context.Context, model string, audio []byte, filename, language string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"capability": "transcribe",
		"model":      model,
		"request": map[string]any{
			"audio_b64": base64.StdEncoding.EncodeToString(audio),
			"filename":  filename, "language": language,
		},
	})
	resp, err := CallBox(ctx, GatewayBoxID, http.MethodPost, "/api/ai-gateway/route",
		bytes.NewReader(payload), "application/json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Text string `json:"text"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return "", fmt.Errorf("gateway transcribe decode: %w", err)
	}
	if !env.Success {
		return "", fmt.Errorf("gateway transcribe: %s", env.Error)
	}
	return env.Data.Text, nil
}
