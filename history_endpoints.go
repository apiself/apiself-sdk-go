package sdk

// HTTP layer over HistoryStore. Mounts under /api/history:
//
//	GET    /api/history                 ?q&model&favorite=1&ok=1&limit&offset
//	GET    /api/history/stats           dashboard KPI (total/today/failedToday)
//	GET    /api/history/retention       current policy
//	POST   /api/history/retention       {maxAgeDays, maxTotalMB}
//	GET    /api/history/file/{id}       full render bytes
//	GET    /api/history/thumb/{id}      thumbnail (jpeg)
//	GET    /api/history/{id}            one row
//	POST   /api/history/{id}/favorite   {favorite: bool}
//	DELETE /api/history/{id}            row + files
//
// Wire in main.go AFTER RegisterRequiredEndpoints:
//
//	hist, _ := sdk.NewHistoryStore(db, cfg.ID, "image")
//	sdk.RegisterHistoryEndpoints(mux, hist)
//	hist.StartRetentionLoop(0)

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type historyHandlers struct{ s *HistoryStore }

// RegisterHistoryEndpoints mounts the /api/history routes.
func RegisterHistoryEndpoints(mux *http.ServeMux, store *HistoryStore) {
	if mux == nil || store == nil {
		return
	}
	h := &historyHandlers{s: store}
	mux.HandleFunc("/api/history", h.list)
	mux.HandleFunc("/api/history/stats", h.stats)
	mux.HandleFunc("/api/history/retention", h.retention)
	mux.HandleFunc("/api/history/file/", h.file)
	mux.HandleFunc("/api/history/thumb/", h.thumb)
	mux.HandleFunc("/api/history/", h.byID)
}

func (h *historyHandlers) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func (h *historyHandlers) fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (h *historyHandlers) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
		return
	}
	q := r.URL.Query()
	f := HistoryFilter{
		Query:        q.Get("q"),
		Model:        q.Get("model"),
		FavoriteOnly: q.Get("favorite") == "1",
		OKOnly:       q.Get("ok") == "1",
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	items, total, err := h.s.List(f)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "history.list_failed")
		return
	}
	h.ok(w, map[string]any{"items": items, "total": total})
}

func (h *historyHandlers) stats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
		return
	}
	h.ok(w, h.s.Stats())
}

func (h *historyHandlers) retention(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.ok(w, h.s.Retention())
	case http.MethodPost:
		var pol HistoryRetention
		if err := json.NewDecoder(r.Body).Decode(&pol); err != nil {
			h.fail(w, http.StatusBadRequest, "history.bad_request")
			return
		}
		if pol.MaxAgeDays < 0 || pol.MaxTotalMB < 0 {
			h.fail(w, http.StatusBadRequest, "history.bad_request")
			return
		}
		if err := h.s.SetRetention(pol); err != nil {
			h.fail(w, http.StatusInternalServerError, "history.save_failed")
			return
		}
		go h.s.RunRetention()
		h.ok(w, pol)
	default:
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
	}
}

// idFromPath extracts the numeric id after the given prefix, tolerating a
// trailing sub-path ("/api/history/12/favorite" -> 12, "favorite").
func idFromPath(path, prefix string) (int64, string) {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.Trim(rest, "/")
	part, sub, _ := strings.Cut(rest, "/")
	id, err := strconv.ParseInt(part, 10, 64)
	if err != nil {
		return 0, ""
	}
	return id, sub
}

func (h *historyHandlers) serveRecordFile(w http.ResponseWriter, r *http.Request, rel string) {
	abs, err := h.s.ResolveFile(rel)
	if err != nil {
		h.fail(w, http.StatusNotFound, "history.file_not_found")
		return
	}
	http.ServeFile(w, r, abs)
}

func (h *historyHandlers) file(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
		return
	}
	id, _ := idFromPath(r.URL.Path, "/api/history/file/")
	rec, err := h.s.Get(id)
	if err != nil || rec.FilePath == "" || rec.FileDeleted {
		h.fail(w, http.StatusNotFound, "history.file_not_found")
		return
	}
	h.serveRecordFile(w, r, rec.FilePath)
}

func (h *historyHandlers) thumb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
		return
	}
	id, _ := idFromPath(r.URL.Path, "/api/history/thumb/")
	rec, err := h.s.Get(id)
	if err != nil || rec.ThumbPath == "" {
		h.fail(w, http.StatusNotFound, "history.file_not_found")
		return
	}
	h.serveRecordFile(w, r, rec.ThumbPath)
}

func (h *historyHandlers) byID(w http.ResponseWriter, r *http.Request) {
	id, sub := idFromPath(r.URL.Path, "/api/history/")
	if id == 0 {
		h.fail(w, http.StatusNotFound, "history.not_found")
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		rec, err := h.s.Get(id)
		if err != nil {
			h.fail(w, http.StatusNotFound, "history.not_found")
			return
		}
		h.ok(w, rec)
	case sub == "" && r.Method == http.MethodDelete:
		if err := h.s.Delete(id); err != nil {
			h.fail(w, http.StatusNotFound, "history.not_found")
			return
		}
		h.ok(w, map[string]bool{"deleted": true})
	case sub == "favorite" && r.Method == http.MethodPost:
		var body struct {
			Favorite bool `json:"favorite"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			h.fail(w, http.StatusBadRequest, "history.bad_request")
			return
		}
		if err := h.s.SetFavorite(id, body.Favorite); err != nil {
			h.fail(w, http.StatusInternalServerError, "history.save_failed")
			return
		}
		h.ok(w, map[string]bool{"favorite": body.Favorite})
	default:
		h.fail(w, http.StatusMethodNotAllowed, "history.method_not_allowed")
	}
}
