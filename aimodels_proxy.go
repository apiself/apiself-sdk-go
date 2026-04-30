package sdk

// Reverse-proxy helper for AI boxes.
//
// The AIModelPicker SDK UI component lives inside box pages and calls
// `fetch('/api/ai-models/...')`. Box pages are served from the box's own
// HTTP port, so without a proxy the request hits the box's catch-all SPA
// route and returns index.html — JSON.parse then chokes on '<' at byte 4.
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
)

// RegisterAIModelProxy mounts /api/ai-models/* and /api/ai-models/manifests/*
// on the given mux as a reverse-proxy to the manager. Idempotent — call it
// once. Panics on a bad APISELF_CORE_URL since that's a config error the
// operator needs to fix before the box can do anything useful.
func RegisterAIModelProxy(mux *http.ServeMux) {
	target, err := url.Parse(GetCoreURL())
	if err != nil {
		// This shouldn't happen in practice — GetCoreURL falls back to a
		// hard-coded http://localhost:7474 default.
		panic("sdk: invalid APISELF_CORE_URL: " + err.Error())
	}
	proxy := httputil.NewSingleHostReverseProxy(target)

	// Default Director sets URL.Scheme + URL.Host but leaves Request.Host
	// pointing at the box's own host header — manager's CORS / auth check
	// this. We override to set it to the manager's host so the request
	// looks identical to a direct call.
	defaultDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		defaultDirector(req)
		req.Host = target.Host
		// Drop any cookies / auth headers the browser carries — manager
		// trusts loopback origin without auth, and forwarding stale tokens
		// could break redirect flows.
		req.Header.Del("Cookie")
		req.Header.Del("Authorization")
		// Path stays /api/ai-models/... — the manager mounts the same
		// prefix, so no rewrite is needed.
	}

	mux.Handle("/api/ai-models/", proxy)
}
