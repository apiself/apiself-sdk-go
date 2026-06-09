package sdk

// Reverse-proxy helper for AI boxes.
//
// The AIModelPicker SDK UI component lives inside box pages and calls
// `fetch('/api/ai-models/...')`. Box pages are served from the box's own
// HTTP port, so without a proxy the request hits the box's catch-all SPA
// route and returns index.html - JSON.parse then chokes on '<' at byte 4.
//
// Boxes that mount the picker call RegisterAIModelProxy(mux) once during
// startup. The proxy forwards every /api/ai-models/* request to the
// manager (resolved via APISELF_CORE_URL or the localhost:7474 default),
// rewriting Host so the manager's auth + CORS + SSEHub broadcast all
// behave identically to a direct manager call.

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

// RegisterAIModelProxy mounts /api/ai-models/* and /api/ai-models/manifests/*
// on the given mux as a reverse-proxy to the manager. Idempotent - call it
// once. Panics on a bad APISELF_CORE_URL since that's a config error the
// operator needs to fix before the box can do anything useful.
func RegisterAIModelProxy(mux *http.ServeMux) {
	target, err := url.Parse(GetCoreURL())
	if err != nil {
		// This shouldn't happen in practice - GetCoreURL falls back to a
		// hard-coded http://localhost:7474 default.
		panic("sdk: invalid APISELF_CORE_URL: " + err.Error())
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Default Director sets URL.Scheme + URL.Host but leaves Request.Host
	// pointing at the box's own host header - manager's CORS / auth check
	// this. We override to set it to the manager's host so the request
	// looks identical to a direct call.
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = target.Host
		// Drop browser-supplied auth before injecting our own server-side
		// identity - prevents stale cookies from leaking onto the manager
		// redirect flow.
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		// Manager auth middleware (apiself-manager/internal/api/auth.go)
		// gates every /api/* request when a password is set - including
		// /api/ai-models/*. The session token lives in our env (manager
		// exports APISELF_SESSION_TOKEN to every box it spawns), so we
		// inject it as X-APISelf-Token so the proxied request passes
		// the gate. Without this every model-list / manifest fetch
		// returns 401 and the AIModelPicker shows "Unauthorized".
		if tok := os.Getenv("APISELF_SESSION_TOKEN"); tok != "" {
			req.Header.Set("X-APISelf-Token", tok)
		}
		// Drop the X-Forwarded-For header that Go's httputil.ReverseProxy
		// auto-populates from the original browser's RemoteAddr. Without
		// this Manager's detectSourceType sees the browser's LAN IP in
		// the header and classifies the request as SourceLAN even though
		// the TCP peer (box -> manager) is on loopback. With LANEnabled
		// default-off on desktop installs that produces a hard 403
		// "access denied" the moment the user opens the box UI from any
		// machine other than the manager host. Box-to-manager is always
		// a local server-to-server hop; forwarding the browser identity
		// here serves no purpose and breaks the source classifier.
		req.Header.Del("X-Forwarded-For")
		// Path stays /api/ai-models/... - the manager mounts the same
		// prefix, so no rewrite is needed.
	}

	mux.Handle("/api/ai-models/", proxy)
}
