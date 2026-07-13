package sdk

// Cloud (BYOK) adapters — the shared code half of the AI Studio standard
// (docs/box-ai-studio-spec.md). Each adapter translates a box's task
// (chat / image / tts / transcribe) into one cloud provider's HTTP API,
// using the user's own key. Adapters live once in the SDK so llm, tts,
// image-gen, video-gen and future boxes don't each re-implement OpenAI /
// Anthropic / Gemini / ElevenLabs plumbing.
//
// The base CloudAdapter carries identity + model listing. Capability is
// expressed by the box asserting an adapter to a capability sub-interface
// (ImageAdapter, ChatAdapter, ...) — added incrementally.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CloudModel is one model a provider offers.
type CloudModel struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// CloudAdapter is the base every provider adapter implements.
type CloudAdapter interface {
	ID() string         // "openai"
	Capability() string // "chat" | "image" | "tts" | "transcribe"
	// Models lists the provider's usable models. Static adapters ignore key;
	// dynamic ones may call the provider's list endpoint with it.
	Models(ctx context.Context, key string) ([]CloudModel, error)
}

// ImageRequest / ChatRequest are the capability-specific call payloads.
type ImageRequest struct {
	Model  string
	Prompt string
	Size   string // e.g. "1024x1024"
}

// ImageAdapter is a CloudAdapter that can generate images.
type ImageAdapter interface {
	CloudAdapter
	Generate(ctx context.Context, key string, req ImageRequest) (png []byte, err error)
}

// TranscribeRequest is the capability-specific payload for speech-to-text.
type TranscribeRequest struct {
	Model    string
	Audio    []byte // the audio file bytes
	Filename string // e.g. "audio.mp3" — provider infers format from extension
	Language string // optional ISO-639-1 code, e.g. "sk"
}

// TranscribeAdapter is a CloudAdapter that can transcribe audio to text.
type TranscribeAdapter interface {
	CloudAdapter
	Transcribe(ctx context.Context, key string, req TranscribeRequest) (text string, err error)
}

// ── registry ─────────────────────────────────────────────────────────────

var (
	cloudMu       sync.RWMutex
	cloudAdapters = map[string]CloudAdapter{}
)

// RegisterCloudAdapter adds an adapter to the process registry. Built-in
// adapters register themselves in init(); boxes may register extras.
func RegisterCloudAdapter(a CloudAdapter) {
	cloudMu.Lock()
	defer cloudMu.Unlock()
	cloudAdapters[a.ID()] = a
}

// CloudAdapterByID returns a registered adapter, or (nil, false).
func CloudAdapterByID(id string) (CloudAdapter, bool) {
	cloudMu.RLock()
	defer cloudMu.RUnlock()
	a, ok := cloudAdapters[id]
	return a, ok
}

// AdapterSupports reports whether an adapter can serve a capability, by checking
// the capability sub-interface it implements. One adapter may implement several
// (e.g. the gemini adapter does both image and video), so this — not the single
// Capability() string — is the authoritative "can it do X" check. Falls back to
// the Capability() string for capabilities without a dedicated sub-interface yet
// (chat/tts/transcribe are served differently today).
func AdapterSupports(a CloudAdapter, capability string) bool {
	if a == nil {
		return false
	}
	switch capability {
	case "image":
		_, ok := a.(ImageAdapter)
		return ok
	case "video":
		_, ok := a.(VideoAdapter)
		return ok
	case "transcribe":
		_, ok := a.(TranscribeAdapter)
		return ok
	default:
		return a.Capability() == capability
	}
}

// CloudAdapters returns all registered adapters sorted by id (stable order).
func CloudAdapters() []CloudAdapter {
	cloudMu.RLock()
	defer cloudMu.RUnlock()
	out := make([]CloudAdapter, 0, len(cloudAdapters))
	for _, a := range cloudAdapters {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// ── OpenAI (images) ──────────────────────────────────────────────────────

func init() { RegisterCloudAdapter(openAIImages{}) }

// openAIImages implements ImageAdapter against the OpenAI images API.
type openAIImages struct{}

func (openAIImages) ID() string         { return "openai" }
func (openAIImages) Capability() string { return "image" }

func (openAIImages) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "gpt-image-1", Label: "GPT Image 1"},
		{ID: "dall-e-3", Label: "DALL·E 3"},
		{ID: "dall-e-2", Label: "DALL·E 2"},
	}, nil
}

func (openAIImages) Generate(ctx context.Context, key string, req ImageRequest) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("openai: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "dall-e-3"
	}
	size := req.Size
	if size == "" {
		size = "1024x1024"
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "prompt": req.Prompt, "n": 1, "size": size, "response_format": "b64_json",
	})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+key)
	cl := &http.Client{Timeout: 120 * time.Second}
	res, err := cl.Do(hr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, fmt.Errorf("openai: %s", msg)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 || out.Data[0].B64 == "" {
		return nil, fmt.Errorf("openai: empty response")
	}
	return base64.StdEncoding.DecodeString(out.Data[0].B64)
}

// ── OpenAI (transcribe — Whisper / gpt-4o-transcribe) ────────────────────
//
// The openai adapter (registered above for images) also implements
// TranscribeAdapter: POST the audio as multipart/form-data to
// /v1/audio/transcriptions and read back the plain text. One "openai" adapter
// serves image + transcribe — AdapterSupports keys off the sub-interface.
func (openAIImages) Transcribe(ctx context.Context, key string, req TranscribeRequest) (string, error) {
	if key == "" {
		return "", fmt.Errorf("openai: no API key configured")
	}
	if len(req.Audio) == 0 {
		return "", fmt.Errorf("openai: no audio provided")
	}
	model := req.Model
	if model == "" {
		model = "gpt-4o-transcribe"
	}
	filename := req.Filename
	if filename == "" {
		filename = "audio.mp3"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(req.Audio); err != nil {
		return "", err
	}
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("response_format", "json")
	if req.Language != "" {
		_ = mw.WriteField("language", req.Language)
	}
	_ = mw.Close()

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.openai.com/v1/audio/transcriptions", &buf)
	if err != nil {
		return "", err
	}
	hr.Header.Set("Content-Type", mw.FormDataContentType())
	hr.Header.Set("Authorization", "Bearer "+key)
	cl := &http.Client{Timeout: 180 * time.Second}
	res, err := cl.Do(hr)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return "", fmt.Errorf("openai: %s", msg)
	}
	var out struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return "", fmt.Errorf("openai: bad transcription response")
	}
	return out.Text, nil
}

// ── Google Gemini (images — "Nano Banana") ───────────────────────────────

func init() { RegisterCloudAdapter(geminiImages{}) }

// geminiImages implements ImageAdapter against the Gemini generateContent
// API. gemini-2.5-flash-image is Google's image model (nicknamed "Nano
// Banana"); it returns the image as an inlineData part.
type geminiImages struct{}

func (geminiImages) ID() string         { return "gemini" }
func (geminiImages) Capability() string { return "image" }

func (geminiImages) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "gemini-2.5-flash-image", Label: "Gemini 2.5 Flash Image (Nano Banana)"},
	}, nil
}

func (geminiImages) Generate(ctx context.Context, key string, req ImageRequest) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("gemini: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "gemini-2.5-flash-image"
	}
	// Documented form for gemini-2.5-flash-image: just the prompt; the image
	// model returns the picture as an inlineData part. (Do NOT set
	// responseModalities here — the dedicated image model returns text-only
	// when it's constrained the wrong way.)
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]any{{"text": req.Prompt}}},
		},
	})
	url := "https://generativelanguage.googleapis.com/v1beta/models/" + model + ":generateContent"
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("x-goog-api-key", key)
	cl := &http.Client{Timeout: 120 * time.Second}
	res, err := cl.Do(hr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, fmt.Errorf("gemini: %s", msg)
	}
	// Accept both camelCase (REST) and snake_case (some responses) for the
	// inline image bytes.
	var out struct {
		Candidates []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Parts []struct {
					Text        string `json:"text"`
					InlineData  *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
					InlineData2 *struct {
						MimeType string `json:"mime_type"`
						Data     string `json:"data"`
					} `json:"inline_data"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("gemini: bad response")
	}
	var textParts []string
	for _, c := range out.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				return base64.StdEncoding.DecodeString(p.InlineData.Data)
			}
			if p.InlineData2 != nil && p.InlineData2.Data != "" {
				return base64.StdEncoding.DecodeString(p.InlineData2.Data)
			}
			if p.Text != "" {
				textParts = append(textParts, p.Text)
			}
		}
	}
	// No image — surface why (safety block, or the model replied with text).
	if out.PromptFeedback.BlockReason != "" {
		return nil, fmt.Errorf("gemini: blocked (%s)", out.PromptFeedback.BlockReason)
	}
	if len(textParts) > 0 {
		msg := strings.Join(textParts, " ")
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("gemini: model returned text, no image — %s", msg)
	}
	return nil, fmt.Errorf("gemini: no image in response")
}

// parseImageSize splits an "WxH" size (e.g. "1024x1024") into pixels, falling
// back to 1024×1024 when the value is empty or malformed.
func parseImageSize(size string) (w, h int) {
	w, h = 1024, 1024
	if i := strings.IndexAny(size, "xX"); i > 0 {
		if a, err := strconv.Atoi(strings.TrimSpace(size[:i])); err == nil && a > 0 {
			w = a
		}
		if b, err := strconv.Atoi(strings.TrimSpace(size[i+1:])); err == nil && b > 0 {
			h = b
		}
	}
	return
}

// ── Stability AI (images — v2beta stable-image) ──────────────────────────

func init() { RegisterCloudAdapter(stabilityImages{}) }

// stabilityImages implements ImageAdapter against Stability's v2beta
// stable-image API. Ultra/Core have dedicated endpoints; the SD3.5 family
// shares /generate/sd3 with a `model` form field. Returns raw PNG bytes
// (Accept: image/*).
type stabilityImages struct{}

func (stabilityImages) ID() string         { return "stability" }
func (stabilityImages) Capability() string { return "image" }

func (stabilityImages) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "stable-image-ultra", Label: "Stable Image Ultra"},
		{ID: "stable-image-core", Label: "Stable Image Core"},
		{ID: "sd3.5-large", Label: "Stable Diffusion 3.5 Large"},
		{ID: "sd3.5-medium", Label: "Stable Diffusion 3.5 Medium"},
	}, nil
}

func (stabilityImages) Generate(ctx context.Context, key string, req ImageRequest) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("stability: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "stable-image-core"
	}
	// Endpoint + optional sd3 model field, chosen from the model id.
	var path, sd3Model string
	switch {
	case strings.Contains(model, "ultra"):
		path = "ultra"
	case strings.Contains(model, "core"):
		path = "core"
	default:
		path, sd3Model = "sd3", model
	}
	// Stability wants multipart/form-data (even without an image field).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", req.Prompt)
	_ = mw.WriteField("output_format", "png")
	if sd3Model != "" {
		_ = mw.WriteField("model", sd3Model)
	}
	_ = mw.Close()

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.stability.ai/v2beta/stable-image/generate/"+path, &buf)
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", mw.FormDataContentType())
	hr.Header.Set("Authorization", "Bearer "+key)
	hr.Header.Set("Accept", "image/*") // raw bytes back, not base64 JSON
	cl := &http.Client{Timeout: 120 * time.Second}
	res, err := cl.Do(hr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode != http.StatusOK {
		// Errors come back as JSON {errors:[...]} or {message}.
		var e struct {
			Errors  []string `json:"errors"`
			Message string   `json:"message"`
			Name    string   `json:"name"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if msg == "" && len(e.Errors) > 0 {
			msg = strings.Join(e.Errors, "; ")
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, fmt.Errorf("stability: %s", msg)
	}
	return raw, nil
}

// ── Black Forest Labs (images — FLUX, async poll) ────────────────────────

func init() { RegisterCloudAdapter(bflImages{}) }

// bflImages implements ImageAdapter against the BFL (FLUX) API. Generation is
// asynchronous: POST returns a request id + polling_url; we poll until the
// result carries a sample URL, then download the image. The blocking
// ImageAdapter.Generate contract is satisfied by doing the poll+download here.
type bflImages struct{}

func (bflImages) ID() string         { return "bfl" }
func (bflImages) Capability() string { return "image" }

func (bflImages) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "flux-pro-1.1", Label: "FLUX Pro 1.1"},
		{ID: "flux-pro", Label: "FLUX Pro"},
		{ID: "flux-dev", Label: "FLUX Dev"},
	}, nil
}

func (bflImages) Generate(ctx context.Context, key string, req ImageRequest) ([]byte, error) {
	if key == "" {
		return nil, fmt.Errorf("bfl: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "flux-pro-1.1"
	}
	w, h := parseImageSize(req.Size)
	body, _ := json.Marshal(map[string]any{"prompt": req.Prompt, "width": w, "height": h})
	sr, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.bfl.ai/v1/"+model, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	sr.Header.Set("Content-Type", "application/json")
	sr.Header.Set("x-key", key) // BFL uses x-key, not Bearer
	cl := &http.Client{Timeout: 30 * time.Second}
	res, err := cl.Do(sr)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("bfl: submit HTTP %d %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}
	var sub struct {
		ID         string `json:"id"`
		PollingURL string `json:"polling_url"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return nil, fmt.Errorf("bfl: no request id in submit response")
	}
	pollURL := sub.PollingURL
	if pollURL == "" {
		pollURL = "https://api.bfl.ai/v1/get_result?id=" + url.QueryEscape(sub.ID)
	}
	// Poll until Ready (or Error), up to ~90s.
	deadline := time.Now().Add(90 * time.Second)
	for {
		pr, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		if err != nil {
			return nil, err
		}
		pr.Header.Set("x-key", key)
		presp, err := cl.Do(pr)
		if err != nil {
			return nil, err
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 4<<20))
		presp.Body.Close()
		var pv struct {
			Status string `json:"status"`
			Result struct {
				Sample string `json:"sample"`
			} `json:"result"`
		}
		_ = json.Unmarshal(pbody, &pv)
		switch pv.Status {
		case "Ready":
			if pv.Result.Sample == "" {
				return nil, fmt.Errorf("bfl: ready but no image url")
			}
			return downloadBytes(ctx, pv.Result.Sample)
		case "Error", "Content Moderated", "Request Moderated", "Failed":
			return nil, fmt.Errorf("bfl: generation %s", pv.Status)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("bfl: timed out waiting for image")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1500 * time.Millisecond):
		}
	}
}

// ── Video (async: submit → poll → download mp4) ──────────────────────────

// VideoRequest is the capability-specific payload for text-to-video.
type VideoRequest struct {
	Model       string
	Prompt      string
	AspectRatio string // "16:9"
	DurationSec int    // 5
	Resolution  string // "720p"
}

// VideoAdapter is a CloudAdapter that can generate a video. Generation is
// inherently asynchronous at every provider (a job that takes tens of seconds
// to minutes); GenerateVideo blocks: it submits, polls until ready, downloads
// the result and returns the raw video bytes + content type.
type VideoAdapter interface {
	CloudAdapter
	GenerateVideo(ctx context.Context, key string, req VideoRequest) (video []byte, contentType string, err error)
}

func vAspect(req VideoRequest) string {
	if req.AspectRatio != "" {
		return req.AspectRatio
	}
	return "16:9"
}
func vDurationSec(req VideoRequest) int {
	if req.DurationSec > 0 {
		return req.DurationSec
	}
	return 5
}

// ── Luma Dream Machine (video — direct API) ──────────────────────────────

func init() { RegisterCloudAdapter(lumaVideo{}) }

type lumaVideo struct{}

func (lumaVideo) ID() string         { return "luma" }
func (lumaVideo) Capability() string { return "video" }

func (lumaVideo) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "ray-2", Label: "Ray 2"},
		{ID: "ray-flash-2", Label: "Ray Flash 2"},
		{ID: "ray-1-6", Label: "Ray 1.6"},
	}, nil
}

func (lumaVideo) GenerateVideo(ctx context.Context, key string, req VideoRequest) ([]byte, string, error) {
	if key == "" {
		return nil, "", fmt.Errorf("luma: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "ray-2"
	}
	res := req.Resolution
	if res == "" {
		res = "720p"
	}
	body, _ := json.Marshal(map[string]any{
		"prompt": req.Prompt, "model": model, "aspect_ratio": vAspect(req),
		"resolution": res, "duration": fmt.Sprintf("%ds", vDurationSec(req)),
	})
	sr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.lumalabs.ai/dream-machine/v1/generations", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	sr.Header.Set("Content-Type", "application/json")
	sr.Header.Set("Authorization", "Bearer "+key)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(sr)
	if err != nil {
		return nil, "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("luma: submit HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var sub struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.ID == "" {
		return nil, "", fmt.Errorf("luma: no generation id in response")
	}
	// Poll GET generations/{id} until state=="completed".
	pollURL := "https://api.lumalabs.ai/dream-machine/v1/generations/" + url.PathEscape(sub.ID)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		pr, _ := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		pr.Header.Set("Authorization", "Bearer "+key)
		presp, err := cl.Do(pr)
		if err != nil {
			return nil, "", err
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 4<<20))
		presp.Body.Close()
		var pv struct {
			State        string `json:"state"`
			FailureReason string `json:"failure_reason"`
			Assets       struct {
				Video string `json:"video"`
			} `json:"assets"`
		}
		_ = json.Unmarshal(pbody, &pv)
		switch pv.State {
		case "completed":
			if pv.Assets.Video == "" {
				return nil, "", fmt.Errorf("luma: completed but no video url")
			}
			vid, err := downloadBytes(ctx, pv.Assets.Video)
			return vid, "video/mp4", err
		case "failed":
			reason := pv.FailureReason
			if reason == "" {
				reason = "failed"
			}
			return nil, "", fmt.Errorf("luma: %s", reason)
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("luma: timed out waiting for video")
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// ── Pika (video — via Fal.ai queue) ──────────────────────────────────────
//
// Pika has no public self-service API; per the product decision the Pika
// provider is served through Fal.ai's uniform queue API. The provider key
// configured for "pika" is therefore a Fal.ai key ("Authorization: Key …").

func init() { RegisterCloudAdapter(pikaVideo{}) }

type pikaVideo struct{}

func (pikaVideo) ID() string         { return "pika" }
func (pikaVideo) Capability() string { return "video" }

func (pikaVideo) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "pika-2.2", Label: "Pika 2.2"},
		{ID: "pika-2.1", Label: "Pika 2.1"},
	}, nil
}

func (pikaVideo) GenerateVideo(ctx context.Context, key string, req VideoRequest) ([]byte, string, error) {
	if key == "" {
		return nil, "", fmt.Errorf("pika: no Fal.ai API key configured")
	}
	// Map catalogue id -> Fal.ai model path.
	falModel := "fal-ai/pika/v2.2/text-to-video"
	if strings.Contains(req.Model, "2.1") {
		falModel = "fal-ai/pika/v2.1/text-to-video"
	}
	res := req.Resolution
	if res == "" {
		res = "720p"
	}
	return falQueueVideo(ctx, key, falModel, map[string]any{
		"prompt": req.Prompt, "aspect_ratio": vAspect(req),
		"resolution": res, "duration": vDurationSec(req),
	})
}

// falQueueVideo runs a Fal.ai queue job to completion and downloads the video.
// Reusable for any Fal-hosted video model (Pika, and later Kling/Runway/Veo if
// routed through Fal). Auth: "Authorization: Key <fal_key>".
func falQueueVideo(ctx context.Context, key, falModel string, input map[string]any) ([]byte, string, error) {
	body, _ := json.Marshal(map[string]any{"input": input})
	sr, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://queue.fal.run/"+falModel, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	sr.Header.Set("Content-Type", "application/json")
	sr.Header.Set("Authorization", "Key "+key)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(sr)
	if err != nil {
		return nil, "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fal: submit HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var sub struct {
		RequestID   string `json:"request_id"`
		StatusURL   string `json:"status_url"`
		ResponseURL string `json:"response_url"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.RequestID == "" {
		return nil, "", fmt.Errorf("fal: no request_id in response")
	}
	statusURL := sub.StatusURL
	if statusURL == "" {
		statusURL = "https://queue.fal.run/" + falModel + "/requests/" + url.PathEscape(sub.RequestID) + "/status"
	}
	respURL := sub.ResponseURL
	if respURL == "" {
		respURL = "https://queue.fal.run/" + falModel + "/requests/" + url.PathEscape(sub.RequestID)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		pr, _ := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		pr.Header.Set("Authorization", "Key "+key)
		presp, err := cl.Do(pr)
		if err != nil {
			return nil, "", err
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 1<<20))
		presp.Body.Close()
		var st struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(pbody, &st)
		if st.Status == "COMPLETED" {
			break
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("fal: timed out waiting for video")
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	// Fetch the result and pull the video URL.
	rr, _ := http.NewRequestWithContext(ctx, http.MethodGet, respURL, nil)
	rr.Header.Set("Authorization", "Key "+key)
	rresp, err := cl.Do(rr)
	if err != nil {
		return nil, "", err
	}
	rbody, _ := io.ReadAll(io.LimitReader(rresp.Body, 4<<20))
	rresp.Body.Close()
	var out struct {
		Video struct {
			URL string `json:"url"`
		} `json:"video"`
	}
	if json.Unmarshal(rbody, &out) != nil || out.Video.URL == "" {
		return nil, "", fmt.Errorf("fal: no video url in result")
	}
	vid, err := downloadBytes(ctx, out.Video.URL)
	return vid, "video/mp4", err
}

// ── Google Gemini Veo (video — direct predictLongRunning API) ────────────
//
// The gemini adapter (registered above for images) ALSO implements VideoAdapter
// via Veo. Submit a long-running operation, poll until done, then download the
// returned file URI (which itself requires the API-key header). One "gemini"
// adapter thus serves image + video — AdapterSupports keys off the sub-interface.
func (geminiImages) GenerateVideo(ctx context.Context, key string, req VideoRequest) ([]byte, string, error) {
	if key == "" {
		return nil, "", fmt.Errorf("gemini: no API key configured")
	}
	model := req.Model
	if model == "" {
		model = "veo-3.1-generate-preview"
	}
	body, _ := json.Marshal(map[string]any{
		"instances":  []map[string]any{{"prompt": req.Prompt}},
		"parameters": map[string]any{"aspectRatio": vAspect(req)},
	})
	sr, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/"+model+":predictLongRunning", bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	sr.Header.Set("Content-Type", "application/json")
	sr.Header.Set("x-goog-api-key", key)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(sr)
	if err != nil {
		return nil, "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("gemini: submit HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var sub struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &sub) != nil || sub.Name == "" {
		return nil, "", fmt.Errorf("gemini: no operation name in response")
	}
	pollURL := "https://generativelanguage.googleapis.com/v1beta/" + sub.Name
	deadline := time.Now().Add(6 * time.Minute)
	for {
		pr, _ := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		pr.Header.Set("x-goog-api-key", key)
		presp, err := cl.Do(pr)
		if err != nil {
			return nil, "", err
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 4<<20))
		presp.Body.Close()
		var pv struct {
			Done  bool `json:"done"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Response struct {
				GVR struct {
					Samples []struct {
						Video struct {
							URI string `json:"uri"`
						} `json:"video"`
					} `json:"generatedSamples"`
				} `json:"generateVideoResponse"`
			} `json:"response"`
		}
		_ = json.Unmarshal(pbody, &pv)
		if pv.Done {
			if pv.Error != nil && pv.Error.Message != "" {
				return nil, "", fmt.Errorf("gemini: %s", pv.Error.Message)
			}
			if len(pv.Response.GVR.Samples) == 0 || pv.Response.GVR.Samples[0].Video.URI == "" {
				return nil, "", fmt.Errorf("gemini: done but no video uri")
			}
			vid, err := downloadBytesHeader(ctx, pv.Response.GVR.Samples[0].Video.URI, "x-goog-api-key", key)
			return vid, "video/mp4", err
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("gemini: timed out waiting for video")
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// ── Kling AI (video — direct, JWT auth) ──────────────────────────────────
//
// Kling authenticates with a short-lived JWT signed (HS256) from an Access Key
// + Secret Key, sent as "Authorization: Bearer <jwt>". The gateway vault holds
// one string per provider, so the configured "kling" key is entered as
// "accessKey:secretKey". NOTE: the create/query request+response shapes below
// follow Kling's documented text2video API but were not doc-verified here
// (their docs are bot-blocked) — worth a runtime check of field names.
func init() { RegisterCloudAdapter(klingVideo{}) }

type klingVideo struct{}

func (klingVideo) ID() string         { return "kling" }
func (klingVideo) Capability() string { return "video" }

func (klingVideo) Models(_ context.Context, _ string) ([]CloudModel, error) {
	return []CloudModel{
		{ID: "kling-v2-master", Label: "Kling v2 Master"},
		{ID: "kling-v1-6", Label: "Kling v1.6"},
		{ID: "kling-v1", Label: "Kling v1"},
	}, nil
}

// klingJWT builds a short-lived HS256 token from "ak:sk".
func klingJWT(key string) (string, error) {
	ak, sk, ok := strings.Cut(key, ":")
	if !ok || ak == "" || sk == "" {
		return "", fmt.Errorf("kling: key must be in the form accessKey:secretKey")
	}
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": ak,
		"exp": now.Add(30 * time.Minute).Unix(),
		"nbf": now.Add(-5 * time.Second).Unix(),
	})
	return tok.SignedString([]byte(sk))
}

func (klingVideo) GenerateVideo(ctx context.Context, key string, req VideoRequest) ([]byte, string, error) {
	token, err := klingJWT(key)
	if err != nil {
		return nil, "", err
	}
	model := req.Model
	if model == "" {
		model = "kling-v1"
	}
	const base = "https://api-singapore.klingai.com/v1/videos/text2video"
	body, _ := json.Marshal(map[string]any{
		"model_name": model, "prompt": req.Prompt,
		"duration": strconv.Itoa(vDurationSec(req)), "aspect_ratio": vAspect(req),
		"mode": "std", "cfg_scale": 0.5,
	})
	sr, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	sr.Header.Set("Content-Type", "application/json")
	sr.Header.Set("Authorization", "Bearer "+token)
	cl := &http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Do(sr)
	if err != nil {
		return nil, "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var sub struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &sub)
	if resp.StatusCode >= 300 || sub.Data.TaskID == "" {
		msg := sub.Message
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return nil, "", fmt.Errorf("kling: %s", msg)
	}
	pollURL := base + "/" + url.PathEscape(sub.Data.TaskID)
	deadline := time.Now().Add(6 * time.Minute)
	for {
		// Re-sign per poll so a long job never uses an expired token.
		ptok, terr := klingJWT(key)
		if terr != nil {
			return nil, "", terr
		}
		pr, _ := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
		pr.Header.Set("Authorization", "Bearer "+ptok)
		presp, err := cl.Do(pr)
		if err != nil {
			return nil, "", err
		}
		pbody, _ := io.ReadAll(io.LimitReader(presp.Body, 4<<20))
		presp.Body.Close()
		var pv struct {
			Data struct {
				TaskStatus    string `json:"task_status"`
				TaskStatusMsg string `json:"task_status_msg"`
				TaskResult    struct {
					Videos []struct {
						URL string `json:"url"`
					} `json:"videos"`
				} `json:"task_result"`
			} `json:"data"`
		}
		_ = json.Unmarshal(pbody, &pv)
		switch pv.Data.TaskStatus {
		case "succeed":
			if len(pv.Data.TaskResult.Videos) == 0 || pv.Data.TaskResult.Videos[0].URL == "" {
				return nil, "", fmt.Errorf("kling: succeeded but no video url")
			}
			vid, err := downloadBytes(ctx, pv.Data.TaskResult.Videos[0].URL)
			return vid, "video/mp4", err
		case "failed":
			msg := pv.Data.TaskStatusMsg
			if msg == "" {
				msg = "failed"
			}
			return nil, "", fmt.Errorf("kling: %s", msg)
		}
		if time.Now().After(deadline) {
			return nil, "", fmt.Errorf("kling: timed out waiting for video")
		}
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// downloadBytes fetches a URL and returns its body (used to pull the final
// image/video off a provider's signed result URL).
func downloadBytes(ctx context.Context, rawURL string) ([]byte, error) {
	return downloadBytesHeader(ctx, rawURL, "", "")
}

// downloadBytesHeader is downloadBytes with an optional auth header (e.g. Google
// file URIs need x-goog-api-key to download).
func downloadBytesHeader(ctx context.Context, rawURL, hKey, hVal string) ([]byte, error) {
	dr, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if hKey != "" {
		dr.Header.Set(hKey, hVal)
	}
	cl := &http.Client{Timeout: 120 * time.Second}
	res, err := cl.Do(dr)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 64<<20))
}
