package sdk

// Box-side helper for accessing shared AI models managed by the manager.
//
// Models live in {DataDir}/shared/ai-models/{family}/{id}.{ext} and are
// shared across boxes — transcribe Pro, future image-gen, future LLM all
// read from the same files. The manager owns download orchestration; this
// helper just resolves paths and (optionally) blocks until a model is
// ready by calling the manager's HTTP API.
//
// Boxes never write into shared/ — the manager is the only writer.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AIModelPath returns the on-disk path of an AI model. If the model is
// already downloaded the path is returned immediately; otherwise the
// helper triggers a download via the manager and blocks until it
// finishes. Use this in box startup code where it's acceptable to wait.
//
// For UI flows that should show download progress, redirect the browser
// to the manager's /api/ai-models/{family}/{id}/download endpoint via
// the AIModelPicker SDK component instead of calling this from JS.
//
// Cancellation is via the (caller-controlled) timeout: pick a duration
// long enough for a 3 GB download on a slow connection (e.g. 30 min).
func AIModelPath(family, id string, timeout time.Duration) (string, error) {
	// Fast path: file exists on disk → no manager round-trip needed.
	if path := localAIModelPath(family, id); path != "" {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	coreURL := GetCoreURL()
	statusURL := fmt.Sprintf("%s/api/ai-models/%s/%s/status", coreURL, family, id)
	downloadURL := fmt.Sprintf("%s/api/ai-models/%s/%s/download", coreURL, family, id)

	state, err := getAIModelStatus(statusURL)
	if err != nil {
		return "", fmt.Errorf("ai-model status: %w", err)
	}
	if state.State == "done" && state.DiskPath != "" {
		return state.DiskPath, nil
	}

	// Trigger the download (idempotent — manager dedupes concurrent calls
	// for the same model). Then poll status until done or failed.
	if state.State != "downloading" {
		req, err := http.NewRequest(http.MethodPost, downloadURL, nil)
		if err != nil {
			return "", err
		}
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("ai-model trigger download: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("ai-model download HTTP %d", resp.StatusCode)
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		state, err = getAIModelStatus(statusURL)
		if err != nil {
			continue // transient — keep polling
		}
		switch state.State {
		case "done":
			return state.DiskPath, nil
		case "failed":
			return "", fmt.Errorf("ai-model download failed: %s", state.Error)
		}
	}
	return "", fmt.Errorf("ai-model %s/%s download timed out after %s", family, id, timeout)
}

// AIModelExists reports whether the model file is on disk WITHOUT
// triggering a download. Cheap stat check suitable for box startup
// banners ("Choose a model" vs "Ready").
func AIModelExists(family, id string) bool {
	p := localAIModelPath(family, id)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

// aiModelStatus mirrors the manager's DownloadState (subset).
type aiModelStatus struct {
	State    string `json:"state"`
	Family   string `json:"family"`
	ID       string `json:"id"`
	Error    string `json:"error,omitempty"`
	DiskPath string `json:"diskPath,omitempty"`
}

type aiModelStatusEnvelope struct {
	Success bool          `json:"success"`
	Data    aiModelStatus `json:"data"`
}

func getAIModelStatus(url string) (*aiModelStatus, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var env aiModelStatusEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("status response not success")
	}
	return &env.Data, nil
}

// localAIModelPath computes the canonical on-disk path WITHOUT consulting
// the manager. Used as a fast pre-check; returns "" if the layout is
// unknown for the OS.
//
// Layout: {DataDir}/shared/ai-models/{family}/{id}.bin (Whisper). For
// other families we fall through and trust the manager's status response.
// This intentionally only knows about the most common case — the ext
// is family-specific (.safetensors, .gguf, .onnx) and lives in the
// manifest. Pre-check is best-effort; the manager is always authoritative.
func localAIModelPath(family, id string) string {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return ""
	}
	root := filepath.Join(dataDir, "shared", "ai-models", family)
	// Probe a few common extensions. Cheap, runs once at box startup.
	for _, ext := range []string{".bin", ".safetensors", ".gguf", ".onnx", ""} {
		candidate := filepath.Join(root, id+ext)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// LooksLikeAIModelID is a loose validator that boxes can use when they
// accept a model ID from user input (URL query param, settings UI). It
// guards against accidental path-injection like "../../etc/passwd"; the
// manager re-validates server-side too, but failing closed earlier gives
// a cleaner error to the caller.
func LooksLikeAIModelID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if strings.ContainsAny(s, "/\\.") {
		return false
	}
	return true
}
