// Cross-box browser-side proxy.
//
// Background: box UIs frequently need to call other boxes through the
// Manager proxy (e.g. filedrop's UI calling /box-notify/api/cb/channels
// to decide whether to render a "Send via Notify" button). When the box
// is opened on its own port for development (e.g. localhost:7500),
// that's a CROSS-ORIGIN request from the browser to the Manager
// (localhost:7474). The whole house of cards then depends on the
// Manager emitting Access-Control-Allow-Origin headers that match what
// the browser expects given the request's `credentials` mode — a
// surprisingly fragile contract that has broken multiple times.
//
// RegisterCrossBoxProxy sidesteps the entire CORS surface by giving the
// host box's BACKEND a same-origin route that forwards to the Manager.
// The UI fetches `/api/cb/proxy/box-{slug}/<path>` against its own
// origin (same-origin → no CORS preflight, no Allow-Origin gymnastics),
// the Go backend then makes a server-to-server call to the Manager,
// and Manager normally proxies to the target box with the usual auth
// header injection (X-APISelf-Caller-Box).
//
// Trust model is preserved: Manager still owns the source-port→box-ID
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
	"strings"
)

const crossBoxProxyPrefix = "/api/cb/proxy/box-"

// RegisterCrossBoxProxy installs the proxy route on the given mux.
// Every path under `/api/cb/proxy/box-<slug>/...` is forwarded to
// `<core-url>/box-<slug>/...`. The forward preserves method, body,
// query string, request headers (except hop-by-hop and Authorization),
// response status, response body, and response headers (except
// hop-by-hop).
//
// Authorization is intentionally stripped from outbound requests
// because Manager replaces it with its own caller identification —
// callers passing through Authorization could otherwise impersonate
// a different box.
func RegisterCrossBoxProxy(mux *http.ServeMux) {
	mux.HandleFunc("/api/cb/proxy/", crossBoxProxyHandler)
}

func crossBoxProxyHandler(w http.ResponseWriter, r *http.Request) {
	// Path must look like /api/cb/proxy/box-<slug>/<rest>
	if !strings.HasPrefix(r.URL.Path, crossBoxProxyPrefix) {
		http.NotFound(w, r)
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/cb/proxy") // /box-<slug>/<rest>
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// Manager unreachable or refused the upstream — treat as 502 so
		// the caller can distinguish from "target box returned 404".
		http.Error(w, "cb_proxy: upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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

// RFC 7230 §6.1 — connection-specific headers that must not be
// forwarded across hops. We also treat "Content-Length" as
// automatically managed by Go's HTTP layer — we don't copy it
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
