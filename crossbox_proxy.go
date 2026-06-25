// Cross-box browser-side proxy.
//
// Background: box UIs frequently need to call other boxes through the
// Manager proxy (e.g. filedrop's UI calling /box-notify/api/cb/channels
// to decide whether to render a "Send via Notify" button). When the box
// is opened on its own port for development (e.g. localhost:7500),
// that's a CROSS-ORIGIN request from the browser to the Manager
// (localhost:7474). The whole house of cards then depends on the
// Manager emitting Access-Control-Allow-Origin headers that match what
// the browser expects given the request's `credentials` mode - a
// surprisingly fragile contract that has broken multiple times.
//
// RegisterCrossBoxProxy sidesteps the entire CORS surface by giving the
// host box's BACKEND a same-origin route that forwards to the Manager.
// The UI fetches `/api/cb/proxy/box-{slug}/<path>` against its own
// origin (same-origin -> no CORS preflight, no Allow-Origin gymnastics),
// the Go backend then makes a server-to-server call to the Manager,
// and Manager normally proxies to the target box with the usual auth
// header injection (X-APISelf-Caller-Box).
//
// Trust model is preserved: Manager still owns the source-port->box-ID
// mapping and injects the caller header itself. We are not bypassing
// the Manager; we are bypassing the BROWSER's CORS layer.
//
// Mount in your box's main.go:
//
//	mux := http.NewServeMux()
//	sdk.RegisterCrossBoxProxy(mux)
//	sdk.RegisterRequiredEndpoints(mux, infoFn)
//
// then point your UI hooks at sdk-ui's `crossBoxBase(boxId)` instead of
// the older `managerBase() + "/box-" + slug`.

package sdk

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
)

const (
	crossBoxProxyBoxPrefix     = "/api/cb/proxy/box-"
	crossBoxProxyManagerPrefix = "/api/cb/proxy/manager/"
)

// cachedBoxID returns this box's own ID (from .apiself/config.json), loaded
// once. Used to stamp the X-APISelf-Caller-Box header so callee boxes can
// attribute a cross-box request to the originating box (e.g. the LLM box
// logging which box's AI assistant called it).
var (
	boxIDOnce sync.Once
	boxIDVal  string
)

func cachedBoxID() string {
	boxIDOnce.Do(func() {
		// Prefer the env var the Manager injects when spawning the box — it's
		// always present and doesn't depend on the process cwd (config-file
		// lookup can miss, which silently dropped the caller header).
		if v := os.Getenv("APISELF_BOX_ID"); v != "" {
			boxIDVal = v
			return
		}
		if cfg, err := LoadConfig(); err == nil {
			boxIDVal = cfg.ID
		}
	})
	return boxIDVal
}

// RegisterCrossBoxProxy installs the proxy route on the given mux.
// TWO upstream targets are supported:
//
//  1. /api/cb/proxy/box-{slug}/<rest>  ->  <core-url>/box-{slug}/<rest>
//     - for calling OTHER boxes through their manager-proxied API.
//
//  2. /api/cb/proxy/manager/<rest>     ->  <core-url>/<rest>
//     - for calling MANAGER ROOT endpoints (e.g.
//     /api/boxes/{id}/availability used by useBoxAvailable). Without
//     this passthrough the box's UI would have to fetch the manager
//     directly with managerBase(), which is cross-origin when the UI
//     is opened on the box's direct port and depends on Manager
//     emitting the right CORS headers.
//
// The forward preserves method, body, query string, request headers
// (except hop-by-hop and Authorization), response status, body and
// response headers (except hop-by-hop).
//
// Authorization is stripped from outbound requests because Manager
// identifies callers by source port, not by client-supplied tokens -
// forwarding a stale or forged Authorization could let one box
// impersonate another.
func RegisterCrossBoxProxy(mux *http.ServeMux) {
	mux.HandleFunc("/api/cb/proxy/", crossBoxProxyHandler)
}

func crossBoxProxyHandler(w http.ResponseWriter, r *http.Request) {
	var tail string
	switch {
	case strings.HasPrefix(r.URL.Path, crossBoxProxyBoxPrefix):
		// /api/cb/proxy/box-<slug>/<rest>  ->  /box-<slug>/<rest>
		tail = strings.TrimPrefix(r.URL.Path, "/api/cb/proxy")
	case strings.HasPrefix(r.URL.Path, crossBoxProxyManagerPrefix):
		// /api/cb/proxy/manager/<rest>  ->  /<rest>
		tail = "/" + strings.TrimPrefix(r.URL.Path, crossBoxProxyManagerPrefix)
	default:
		http.NotFound(w, r)
		return
	}
	upstream := strings.TrimRight(GetCoreURL(), "/") + tail
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, r.Body)
	if err != nil {
		http.Error(w, "cb_proxy: build request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Forward the incoming Content-Length so the upstream gets a proper
	// fixed-length body. Without this, Go's http.Client falls back to
	// chunked transfer encoding (because a non-bytes.Buffer body has
	// ContentLength = 0), and some upstream proxies / hand-written
	// reverse proxies drop the body and the target sees EOF on Decode.
	req.ContentLength = r.ContentLength
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Del("Authorization") // never forward; Manager injects identity
	// Stamp the calling box's ID so the callee can attribute the request
	// (e.g. LLM box usage shows which box's AI assistant called it). The
	// Manager's reverse proxy forwards this header to the target box.
	if id := cachedBoxID(); id != "" {
		req.Header.Set("X-APISelf-Caller-Box", id)
	}
	// Strip reverse-proxy-only headers. Caddy/nginx infront of the Manager
	// set these so the FIRST hop (user -> Manager) is correctly classified as
	// Remote. But this SECOND hop (box -> Manager loopback) is genuinely a
	// localhost call - if we propagate the headers, Manager.detectSourceType
	// honours them and labels the inner call as Remote too. With Remote off
	// in the default access policy, the cross-box call gets 403 even though
	// every IO happened on 127.0.0.1. 2026-05-28 user reported breaks
	// useBoxMaturity, useBoxAvailable and any other manager-root proxy hop.
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("X-Real-IP")
	req.Header.Del("Forwarded")
	// Inter-box auth - boxes that gate their /api/cb/* endpoints (e.g.
	// Storage's CrossBoxSave, Notify's CrossBoxSend) check the
	// X-APISELF-Box-Token header against their own APISELF_SESSION_TOKEN
	// env var. The Manager hands every box the same token via
	// os.Environ() inheritance, so we inject it here so callee boxes
	// can verify the request originated inside the trusted Manager
	// process tree. Without this the proxy returns the callee's 401
	// in dev mode (where the env var is set). In production where the
	// var is unset, the header is empty and the callee's check is a
	// no-op.
	if tok := os.Getenv("APISELF_SESSION_TOKEN"); tok != "" {
		req.Header.Set("X-APISELF-Box-Token", tok)
		// X-APISelf-Token - manager's own auth middleware checks this
		// against local_session_token in the manager DB. Required when
		// the proxy target is /api/cb/proxy/manager/* (manager-root
		// passthrough) and the manager has a password set; also harmless
		// on box-target forwards because Manager strips it before
		// reaching the callee box.
		req.Header.Set("X-APISelf-Token", tok)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Manager unreachable or refused the upstream - treat as 502 so
		// the caller can distinguish from "target box returned 404".
		http.Error(w, "cb_proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// SSE / streaming odpovede (napr. llm box /v1/chat/completions
	// stream:true) musíme flushovať per-chunk, inak io.Copy bufferuje a
	// klient dostane celú odpoveď naraz na konci (streaming "nefunguje").
	if isStreaming(resp.Header) {
		copyFlushing(w, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

func isStreaming(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream") ||
		strings.Contains(ct, "application/x-ndjson") ||
		h.Get("X-Accel-Buffering") == "no"
}

// copyFlushing kopíruje stream a flushuje po každom chunk-u (ak je
// ResponseWriter http.Flusher). Tým sa SSE delty dostanú ku klientovi
// priebežne namiesto naraz.
func copyFlushing(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// RFC 7230 §6.1 - connection-specific headers that must not be
// forwarded across hops. We also treat "Content-Length" as
// automatically managed by Go's HTTP layer - we don't copy it
// explicitly to avoid mismatches when the body changes encoding.
func isHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailers",
		"transfer-encoding", "upgrade":
		return true
	}
	return false
}
