package sdk

// Box-side helper for soft cross-box dependencies.
//
// Pattern: caller box (e.g. transcribe) declares an OPTIONAL dep on
// another box (e.g. converter) in its .apiself/config.json under
// `dependencies.boxes` with `"required": false`. At runtime the caller
// uses IsBoxAvailable() to decide whether to expose a feature, and
// CallBox() to actually invoke the dep when it's available.
//
// Hard deps (`"required": true`) are enforced by the manager at start-up
// — caller doesn't need to check. This helper is purely for soft deps.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// applyManagerAuth sets the manager session-token header from the env
// var the manager injects into every box's process. Manager's auth
// middleware accepts requests with `X-APISelf-Token == local_session_token`
// (see apiself-manager/internal/api/auth.go). In dev mode the env var
// is set (dev.ps1 -> APISELF_SESSION_TOKEN=devtoken) and the same value
// becomes the DB's local_session_token, so the box-side header matches.
// In production the var is unset and manager has no password set, so
// requests pass through without this header anyway — making this a
// no-op rather than a regression.
func applyManagerAuth(req *http.Request) {
	if tok := os.Getenv("APISELF_SESSION_TOKEN"); tok != "" {
		req.Header.Set("X-APISelf-Token", tok)
	}
}

// BoxAvailability mirrors apiself-manager/internal/types.BoxAvailability.
// Kept verbatim so this file compiles without importing the manager
// package (boxes don't depend on the manager binary).
type BoxAvailability struct {
	BoxID     string `json:"boxId"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Version   string `json:"version,omitempty"`
	Reason    string `json:"reason,omitempty"` // "ok" | "not_installed" | "not_running" | "version_too_old"
}

// IsBoxAvailable asks the manager whether `boxID` is installed, running,
// and (optionally) at least at version `since`. Pass empty string for
// `since` to skip the version gate. Pass empty or zero `timeout` to use
// a 2-second default — the manager's local lookup is cheap, this is a
// safety net for stuck connections.
//
// Returns nil + non-nil error on transport failure (manager unreachable
// from inside the box's process — uncommon, both run on localhost).
// Returns the result with Reason="ok" on success.
func IsBoxAvailable(ctx context.Context, boxID, since string, timeout time.Duration) (*BoxAvailability, error) {
	if boxID == "" {
		return nil, fmt.Errorf("IsBoxAvailable: boxID required")
	}
	if timeout == 0 {
		timeout = 2 * time.Second
	}

	endpoint := GetCoreURL() + "/api/boxes/" + url.PathEscape(boxID) + "/availability"
	if since != "" {
		endpoint += "?since=" + url.QueryEscape(since)
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	applyManagerAuth(req)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("availability HTTP: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("availability status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var env struct {
		Success bool             `json:"success"`
		Data    BoxAvailability  `json:"data"`
		Error   string           `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("availability decode: %w", err)
	}
	if !env.Success {
		return nil, fmt.Errorf("availability error: %s", env.Error)
	}
	return &env.Data, nil
}

// CallBox makes an HTTP request to another box via the manager's
// /box-{id}/* reverse proxy. The manager handles routing, port
// resolution, and inter-box auth (it injects an X-APISelf-Caller header
// on requests originating from another box's process — boxes can verify
// that header before serving sensitive endpoints).
//
// The returned response is the raw http.Response — caller is responsible
// for reading and closing the body. Use this when the response is bytes
// (DOCX, PDF, image data, etc.) where automatic JSON parsing would be
// wrong. For JSON responses, parse env.Data yourself with the standard
// envelope:  {success, data, error}.
//
// Recommended pattern:
//
//	avail, _ := sdk.IsBoxAvailable(ctx, "apiself-box-converter", "1.0.0", 0)
//	if avail == nil || avail.Reason != "ok" {
//	    // render locked CTA
//	    return
//	}
//	resp, err := sdk.CallBox(ctx, "apiself-box-converter", http.MethodPost,
//	                        "/api/convert?to=docx", body, "application/markdown")
//	if err != nil { ... }
//	defer resp.Body.Close()
//	io.Copy(w, resp.Body) // stream DOCX to user
func CallBox(ctx context.Context, boxID, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	if boxID == "" {
		return nil, fmt.Errorf("CallBox: boxID required")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Manager exposes other boxes at /box-{slug}/* — slug is the box ID
	// without the apiself-box- prefix. Same convention as elsewhere.
	slug := strings.TrimPrefix(boxID, "apiself-box-")
	endpoint := GetCoreURL() + "/box-" + slug + path

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Manager auth — when bound to anything other than loopback or set up
	// with a password (i.e. production single-user installs), the
	// /box-{slug}/* proxy rejects requests without X-APISelf-Token. Box
	// processes inherit the same APISELF_SESSION_TOKEN value the manager
	// stored as local_session_token, so applying it here lets box→box
	// CallBox traffic pass through that gate.
	applyManagerAuth(req)
	// Also forward the box-token header so the CALLEE box's own
	// authCrossBox check (if it has one) accepts the request without a
	// second config step. Both headers carry the same env value.
	if tok := os.Getenv("APISELF_SESSION_TOKEN"); tok != "" {
		req.Header.Set("X-APISELF-Box-Token", tok)
	}
	// The manager appends X-APISelf-Caller automatically when this request
	// is proxied through it. We don't set it here — the callee should not
	// trust caller-set values.
	return http.DefaultClient.Do(req)
}
