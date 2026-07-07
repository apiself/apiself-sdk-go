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
	"net/http"
	"sort"
	"sync"
	"time"
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
