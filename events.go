package sdk

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Box event stream (Server-Sent Events). Lets a box push live updates to its
// open UI pages instead of the UI polling on an interval. The UI subscribes
// with the sdk-ui useBoxEvents hook (EventSource on /api/events).
//
// Usage:
//   main.go:        sdk.RegisterEventStream(mux)
//   on a change:    sdk.PublishEvent("download", map[string]any{"linkId": id})
//
// This is local to the box (served over the manager proxy, which flushes
// text/event-stream) - it never polls the cloud. Prefer this over interval
// polling for anything that changes server-side (counts, status, stats).

type eventHub struct {
	mu   sync.RWMutex
	subs map[chan []byte]struct{}
}

var boxEventHub = &eventHub{subs: make(map[chan []byte]struct{})}

// PublishEvent broadcasts an event to every connected /api/events subscriber.
// eventType is a short name ("download", "response", ...); data is JSON-encoded
// into the SSE payload. Non-blocking: a slow subscriber's event is dropped
// rather than stalling the publisher.
func PublishEvent(eventType string, data any) {
	payload, err := json.Marshal(map[string]any{"type": eventType, "data": data})
	if err != nil {
		return
	}
	boxEventHub.mu.RLock()
	defer boxEventHub.mu.RUnlock()
	for ch := range boxEventHub.subs {
		select {
		case ch <- payload:
		default: // subscriber buffer full - drop, UI refetches on the next event
		}
	}
}

// RegisterEventStream mounts GET /api/events as an SSE endpoint. Call once in
// main.go (it's a public-ish read stream; the manager proxy gates access like
// any other box route).
func RegisterEventStream(mux *http.ServeMux) {
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		// ResponseController unwraps middleware wrappers (e.g. the box logging
		// wrapper) to reach the underlying Flusher - more robust than a direct
		// w.(http.Flusher) type assertion, which fails behind any wrapper.
		rc := http.NewResponseController(w)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

		ch := make(chan []byte, 32)
		boxEventHub.mu.Lock()
		boxEventHub.subs[ch] = struct{}{}
		boxEventHub.mu.Unlock()
		defer func() {
			boxEventHub.mu.Lock()
			delete(boxEventHub.subs, ch)
			boxEventHub.mu.Unlock()
		}()

		_, _ = w.Write([]byte(": connected\n\n"))
		_ = rc.Flush()

		ctx := r.Context()
		ping := time.NewTicker(25 * time.Second) // keep idle proxies open
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				_, _ = w.Write([]byte("data: "))
				_, _ = w.Write(msg)
				_, _ = w.Write([]byte("\n\n"))
				_ = rc.Flush()
			case <-ping.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				_ = rc.Flush()
			}
		}
	})
}
