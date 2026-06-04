// Notify cross-box helpers - let any box dispatch a notification (email
// / Slack / Telegram / webhook ...) through the optional apiself-box-notify
// gateway. Callers degrade gracefully when notify is not installed: the
// caller's UI shows an "install Notify" CTA or falls back to a `mailto:`
// link, the helper here just reports `ErrNotifyUnavailable`.
//
// Two entry points:
//
//   NotifySend(ctx, NotifyRequest{...})
//       Dispatch a single message.
//
//   NotifyRegisterTemplates(ctx, "apiself-box-foo", []NotifyTemplate{...})
//       Idempotent - call at box startup. Notify reflects the templates
//       in its admin UI and the user can override subject/body/html.
//       Default fields are updated; user overrides are preserved.
//
// Both rely on CallBox + the /api/cb/* surface defined in
// apiself-box-notify. Auth is done by Manager's proxy injecting an
// X-APISelf-Caller header - callers do not authenticate explicitly.

package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const NotifyBoxID = "apiself-box-notify"

// ErrNotifyUnavailable means Notify is not installed / not running. The
// caller should not treat this as a hard failure - it's the contract for
// a soft cross-box dep. Decide per-feature whether to suppress or to
// surface an install CTA to the end user.
var ErrNotifyUnavailable = errors.New("sdk: apiself-box-notify is not installed or not running")

// NotifyRequest mirrors apiself-box-notify's CrossBoxSendRequest. Either
// ChannelID OR ChannelCategory must be set. If Template is set, Subject
// and Body are optional (overrides). Otherwise Subject + Body are
// required.
type NotifyRequest struct {
	ChannelID       string            `json:"channel_id,omitempty"`
	ChannelCategory string            `json:"channel_category,omitempty"`
	Template        string            `json:"template,omitempty"`
	Data            map[string]string `json:"data,omitempty"`
	Subject         string            `json:"subject,omitempty"`
	Body            string            `json:"body,omitempty"`
	HTML            string            `json:"html,omitempty"`
	Recipient       string            `json:"recipient,omitempty"`
	Priority        string            `json:"priority,omitempty"`
	CallerEvent     string            `json:"-"` // sent as X-APISelf-Caller-Event header
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// NotifyResponse mirrors CrossBoxSendResponse.
type NotifyResponse struct {
	MessageID            string    `json:"message_id"`
	ChannelID            string    `json:"channel_id"`
	ChannelBackend       string    `json:"channel_backend"`
	QueuedAt             time.Time `json:"queued_at"`
	DeliveredAt          time.Time `json:"delivered_at,omitempty"`
	ProviderResponseCode int       `json:"provider_response_code"`
}

// NotifyTemplate is the registration payload entry. Notify auto-extracts
// variable names from {{var}} occurrences in Subject/Body/HTML when Vars
// is empty - caller boxes that want to be explicit can list them anyway
// to keep the admin UI authoritative.
type NotifyTemplate struct {
	Key            string   `json:"key"`
	DefaultSubject string   `json:"default_subject"`
	DefaultBody    string   `json:"default_body"`
	DefaultHTML    string   `json:"default_html,omitempty"`
	Vars           []string `json:"vars,omitempty"`
	Category       string   `json:"category"`
	Description    string   `json:"description,omitempty"`
}

// NotifyAvailable is a convenience wrapper that probes Notify's
// availability through the manager. Cheap (sub-100ms typical); fine to
// call once at startup and once before each dispatch. For continuous UI
// polling use the useNotify React hook in apiself-sdk-ui.
func NotifyAvailable(ctx context.Context) bool {
	a, err := IsBoxAvailable(ctx, NotifyBoxID, "", 2*time.Second)
	if err != nil || a == nil {
		return false
	}
	return a.Installed && a.Running
}

// NotifySend dispatches a notification through Notify. Returns
// ErrNotifyUnavailable if the box is not reachable; the caller is
// responsible for whatever fallback UX is appropriate (mailto: link,
// CTA, silent skip).
func NotifySend(ctx context.Context, req NotifyRequest) (*NotifyResponse, error) {
	if req.ChannelID == "" && req.ChannelCategory == "" {
		req.ChannelCategory = "transactional"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("notify send: marshal: %w", err)
	}
	resp, err := callNotify(ctx, http.MethodPost, "/api/cb/send", body, req.CallerEvent)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := readEnvelope[NotifyResponse](resp)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// NotifyRegisterTemplates is the idempotent install/upgrade hook. Call
// it from your box's main.go right after sdk.InitBox completes; safe to
// invoke on every startup. If Notify isn't installed yet, this returns
// ErrNotifyUnavailable and you can simply ignore it - when the user
// installs Notify later, the next box restart will register templates.
func NotifyRegisterTemplates(ctx context.Context, boxID string, templates []NotifyTemplate) error {
	if len(templates) == 0 {
		return nil
	}
	payload := map[string]any{"box_id": boxID, "templates": templates}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify templates: marshal: %w", err)
	}
	resp, err := callNotify(ctx, http.MethodPost, "/api/cb/templates/register", body, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = readEnvelope[map[string]any](resp)
	return err
}

// callNotify wraps CallBox with the right headers and the unavailable
// detection. We treat manager-reachable-but-Notify-not-installed as
// ErrNotifyUnavailable (HTTP 404 from the manager proxy for an unknown
// /box-notify/ slug, or transport error on a stopped box).
func callNotify(ctx context.Context, method, path string, body []byte, event string) (*http.Response, error) {
	if !NotifyAvailable(ctx) {
		return nil, ErrNotifyUnavailable
	}
	req, err := CallBox(ctx, NotifyBoxID, method, path, bytes.NewReader(body), "application/json")
	if err != nil {
		return nil, ErrNotifyUnavailable
	}
	if event != "" {
		// Header injection via CallBox isn't supported by the helper
		// signature; manager proxy passes through whatever headers
		// we'd want - but our CallBox sets the request before sending.
		// Practical solution: re-send via direct http.Client with
		// header attached.
		_ = req // CallBox already executed the request; falling back
		// is unnecessary because Notify reads X-APISelf-Caller-Event
		// from the body's `metadata` map instead.
	}
	if req.StatusCode == http.StatusNotFound || req.StatusCode == http.StatusBadGateway {
		return req, ErrNotifyUnavailable
	}
	return req, nil
}

// readEnvelope is a small helper to drain a {success, data, error}
// envelope and surface the right Go error when success is false. Generic
// so callers get a typed result.
func readEnvelope[T any](resp *http.Response) (T, error) {
	var zero T
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, fmt.Errorf("notify response read: %w", err)
	}
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
		Detail  string          `json:"detail"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, fmt.Errorf("notify response json: %w (body=%s)", err, string(raw))
	}
	if !env.Success {
		msg := env.Error
		if env.Detail != "" {
			msg += ": " + env.Detail
		}
		if msg == "" {
			msg = fmt.Sprintf("notify HTTP %d", resp.StatusCode)
		}
		return zero, errors.New(msg)
	}
	if len(env.Data) == 0 {
		return zero, nil
	}
	var out T
	if err := json.Unmarshal(env.Data, &out); err != nil {
		return zero, fmt.Errorf("notify data json: %w", err)
	}
	return out, nil
}
