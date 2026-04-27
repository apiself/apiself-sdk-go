// Package locale provides i18n utilities for APISelf boxes.
//
// Boxes embed their own translation files and use this package
// via the SDK-provided T() helper and Middleware.
//
// Typical box setup:
//
//	//go:embed i18n/active.en.json i18n/active.sk.json
//	var localeFS embed.FS
//
//	func main() {
//	    bundle := locale.NewBundle(localeFS, "i18n/active.en.json", "i18n/active.sk.json")
//	    handler = locale.Middleware(bundle)(handler)
//	}
package locale

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

type ctxKey struct{}

// Bundle wraps i18n.Bundle for use in boxes.
type Bundle struct {
	b        *i18n.Bundle
	fallback *i18n.Localizer
}

// NewBundle initialises an i18n bundle from the provided fs.FS and file paths.
// Falls back to English if a language is not found.
//
// Example:
//
//	//go:embed i18n/active.en.json i18n/active.sk.json
//	var localeFS embed.FS
//
//	bundle := locale.NewBundle(localeFS, "i18n/active.en.json", "i18n/active.sk.json")
func NewBundle(fsys fs.FS, files ...string) *Bundle {
	b := i18n.NewBundle(language.English)
	b.RegisterUnmarshalFunc("json", json.Unmarshal)
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			slog.Warn("locale: cannot read file", "file", f, "err", err)
			continue
		}
		if _, err := b.ParseMessageFileBytes(data, f); err != nil {
			slog.Warn("locale: cannot parse file", "file", f, "err", err)
		}
	}
	return &Bundle{
		b:        b,
		fallback: i18n.NewLocalizer(b, language.English.String()),
	}
}

// Middleware detects language from Accept-Language header or ?lang= query param
// and stores a localizer in the request context.
func (bnd *Bundle) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := r.Header.Get("Accept-Language")
		if lang == "" {
			lang = r.URL.Query().Get("lang")
		}
		loc := i18n.NewLocalizer(bnd.b, lang, language.English.String())
		ctx := context.WithValue(r.Context(), ctxKey{}, loc)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WithLang returns a context with a localizer for the given language code.
func (bnd *Bundle) WithLang(ctx context.Context, lang string) context.Context {
	loc := i18n.NewLocalizer(bnd.b, lang, language.English.String())
	return context.WithValue(ctx, ctxKey{}, loc)
}

// T returns the localized message for msgID.
// Falls back to English, then to msgID itself if the ID is not in any bundle.
// data is an optional template data map for messages with {{.Var}} placeholders.
func (bnd *Bundle) T(ctx context.Context, msgID string, data ...map[string]any) string {
	loc, ok := ctx.Value(ctxKey{}).(*i18n.Localizer)
	if !ok || loc == nil {
		loc = bnd.fallback
	}
	cfg := &i18n.LocalizeConfig{MessageID: msgID}
	if len(data) > 0 && data[0] != nil {
		cfg.TemplateData = data[0]
	}
	msg, err := loc.Localize(cfg)
	if err != nil {
		if msg2, err2 := bnd.fallback.Localize(cfg); err2 == nil {
			return msg2
		}
		return msgID
	}
	return msg
}
