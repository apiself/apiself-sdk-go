package sdk

// Shared external-runtime downloader.
//
// Unifies what every box that needs a portable engine (pandoc, chrome,
// llama-cpp, whisper-cpp, python runtimes, ...) previously reimplemented on
// its own: read the box `.apiself/config.json` `dependencies.external[]`, pick
// the download for the current GOOS/GOARCH, download + extract the archive,
// locate the inner binary, and cache it in the SHARED data dir so a runtime
// installed by one box is reused by another (dedup).
//
// Layout (matches the shared/ standard - filesystem-layout.md; same family as
// EnsureModel's shared/ai-models):
//
//	{DataDir}/shared/{name}/{version}/{binary}       - the cached binary
//	{DataDir}/shared/{name}/{version}/.ok            - completion sentinel
//	{DataDir}/shared/{name}/{version}/.lock          - in-flight (cross-process)
//
// Concurrency: an in-process mutex per name + the cross-process .lock/.ok
// sentinels (shared with EnsureModel's acquireDownloadLock/waitForSentinel) so
// two boxes racing to install the same runtime don't download twice.
//
// Progress: PublishEvent("runtime", {name, phase, pct}) with phases
// download/extract/ready/error, consumed by the SDK-UI <RuntimeGate>. A
// successful install is audited via Audit("runtime.download", name, ...).
//
// Deliberately stdlib-only (archive/zip + archive/tar + compress/gzip). tar.xz
// is intentionally unsupported (no engine we ship needs it); add ulikunitz/xz
// here if that changes.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	rtMuxRegistry = map[string]*sync.Mutex{}
	rtMuxRegMu    sync.Mutex
)

func rtLockFor(name string) *sync.Mutex {
	rtMuxRegMu.Lock()
	defer rtMuxRegMu.Unlock()
	m, ok := rtMuxRegistry[name]
	if !ok {
		m = &sync.Mutex{}
		rtMuxRegistry[name] = m
	}
	return m
}

// findExternalDep returns the config.json dependencies.external[] entry with
// the given name (nil if absent). Reads the box config via LoadConfig().
func findExternalDep(name string) (*BoxConfigExternalDep, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("runtime %q: load config: %w", name, err)
	}
	for i := range cfg.Dependencies.External {
		if cfg.Dependencies.External[i].Name == name {
			return &cfg.Dependencies.External[i], nil
		}
	}
	return nil, fmt.Errorf("runtime %q: not declared in dependencies.external", name)
}

// platformKey is the "os-arch" key used in the downloads map.
func platformKey() string { return runtime.GOOS + "-" + runtime.GOARCH }

// runtimeVersion returns the version segment for the shared/ path.
func runtimeVersion(dep *BoxConfigExternalDep) string {
	if dep.Version != "" {
		return dep.Version
	}
	return "latest"
}

// runtimeCacheDir is {DataDir}/shared/{name}/{version}.
func runtimeCacheDir(dep *BoxConfigExternalDep) (string, error) {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return "", fmt.Errorf("runtime %q: cannot resolve data dir", dep.Name)
	}
	return filepath.Join(dataDir, "shared", dep.Name, runtimeVersion(dep)), nil
}

// runtimeBinPath resolves the cached binary path for a dep (may not exist yet).
func runtimeBinPath(dep *BoxConfigExternalDep) (string, error) {
	dl, ok := dep.Downloads[platformKey()]
	if !ok {
		return "", fmt.Errorf("runtime %q: not available on this platform (%s)", dep.Name, platformKey())
	}
	binName := dl.Binary
	if binName == "" {
		return "", fmt.Errorf("runtime %q: no binary path for %s", dep.Name, platformKey())
	}
	cacheDir, err := runtimeCacheDir(dep)
	if err != nil {
		return "", err
	}
	return filepath.Join(cacheDir, filepath.Base(binName)), nil
}

// SharedRuntimePath returns the cached binary path for `name` if it is already
// installed (does not download). ready=false when absent or not on this platform.
func SharedRuntimePath(name string) (string, bool) {
	dep, err := findExternalDep(name)
	if err != nil {
		return "", false
	}
	binPath, err := runtimeBinPath(dep)
	if err != nil {
		return "", false
	}
	cacheDir, _ := runtimeCacheDir(dep)
	if _, err := os.Stat(filepath.Join(cacheDir, ".ok")); err != nil {
		return "", false
	}
	if fi, err := os.Stat(binPath); err != nil || fi.IsDir() {
		return "", false
	}
	return binPath, true
}

// SharedRuntimeInfo returns the config declaration for `name` (for UI: size,
// tier, feature/rationale).
func SharedRuntimeInfo(name string) (BoxConfigExternalDep, bool) {
	dep, err := findExternalDep(name)
	if err != nil {
		return BoxConfigExternalDep{}, false
	}
	return *dep, true
}

// EnsureSharedRuntime guarantees the runtime `name` (declared in
// dependencies.external[]) is installed in the shared data dir and returns the
// absolute path to its binary. Idempotent (cache hit returns immediately),
// cross-process safe, emits "runtime" progress events. Never panics.
func EnsureSharedRuntime(ctx context.Context, name string) (string, error) {
	dep, err := findExternalDep(name)
	if err != nil {
		rtEmit(name, "error", 0)
		return "", err
	}
	dl, ok := dep.Downloads[platformKey()]
	if !ok {
		rtEmit(name, "error", 0)
		return "", fmt.Errorf("runtime %q: not available on this platform (%s)", name, platformKey())
	}
	binName := dl.Binary
	if binName == "" {
		rtEmit(name, "error", 0)
		return "", fmt.Errorf("runtime %q: no binary path for %s", name, platformKey())
	}

	cacheDir, err := runtimeCacheDir(dep)
	if err != nil {
		rtEmit(name, "error", 0)
		return "", err
	}
	cachedBin := filepath.Join(cacheDir, filepath.Base(binName))
	sentinel := filepath.Join(cacheDir, ".ok")

	// Fast path: sentinel + binary present.
	if _, err := os.Stat(sentinel); err == nil {
		if fi, err := os.Stat(cachedBin); err == nil && !fi.IsDir() {
			return cachedBin, nil
		}
	}

	// In-process serialization (this box).
	m := rtLockFor(name)
	m.Lock()
	defer m.Unlock()

	// Re-check after the in-process lock.
	if _, err := os.Stat(sentinel); err == nil {
		if fi, err := os.Stat(cachedBin); err == nil && !fi.IsDir() {
			return cachedBin, nil
		}
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		rtEmit(name, "error", 0)
		return "", fmt.Errorf("runtime %q: create cache dir: %w", name, err)
	}

	// Cross-process lock (other boxes). If a sibling holds it, wait for the
	// sentinel instead of downloading twice.
	lockPath := filepath.Join(cacheDir, ".lock")
	locked, err := acquireDownloadLock(lockPath)
	if err != nil {
		rtEmit(name, "error", 0)
		return "", fmt.Errorf("runtime %q: lock: %w", name, err)
	}
	if !locked {
		if err := waitForSentinel(sentinel, 30*time.Minute); err != nil {
			rtEmit(name, "error", 0)
			return "", err
		}
		return cachedBin, nil
	}
	defer os.Remove(lockPath)

	// Re-check after the cross-process lock.
	if fi, err := os.Stat(cachedBin); err == nil && !fi.IsDir() {
		if _, err := os.Stat(sentinel); err == nil {
			return cachedBin, nil
		}
	}

	tmpDir := filepath.Join(cacheDir, ".tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		rtEmit(name, "error", 0)
		return "", fmt.Errorf("runtime %q: temp dir: %w", name, err)
	}
	defer os.RemoveAll(tmpDir)

	archiveKind := dl.Archive
	if archiveKind == "" {
		archiveKind = rtInferArchive(dl.URL)
	}

	rtEmit(name, "download", 0)
	archivePath, err := rtDownload(ctx, name, dl.URL, tmpDir)
	if err != nil {
		rtEmit(name, "error", 0)
		return "", err
	}

	// "raw" (or no archive) = the download IS the binary; move it straight in.
	if archiveKind == "" || archiveKind == "raw" || archiveKind == "binary" {
		if err := rtMoveFile(archivePath, cachedBin); err != nil {
			rtEmit(name, "error", 0)
			return "", fmt.Errorf("runtime %q: install: %w", name, err)
		}
	} else {
		rtEmit(name, "extract", 0)
		extractDir, err := os.MkdirTemp(tmpDir, name+"-x-")
		if err != nil {
			rtEmit(name, "error", 0)
			return "", fmt.Errorf("runtime %q: extract dir: %w", name, err)
		}
		if err := rtExtract(archivePath, archiveKind, extractDir); err != nil {
			rtEmit(name, "error", 0)
			return "", fmt.Errorf("runtime %q: extract: %w", name, err)
		}
		found, err := rtLocateBinary(extractDir, binName)
		if err != nil {
			rtEmit(name, "error", 0)
			return "", fmt.Errorf("runtime %q: %w", name, err)
		}
		if err := rtMoveFile(found, cachedBin); err != nil {
			rtEmit(name, "error", 0)
			return "", fmt.Errorf("runtime %q: install: %w", name, err)
		}
	}

	if runtime.GOOS != "windows" {
		_ = os.Chmod(cachedBin, 0o755)
	}
	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		_ = err // non-fatal; next call just re-verifies the binary
	}

	rtEmit(name, "ready", 100)
	Audit("runtime.download", name, fmt.Sprintf("%s installed to %s", platformKey(), cacheDir))
	return cachedBin, nil
}

// RegisterRuntimeEndpoints mounts the uniform runtime endpoints:
//
//	GET  /api/runtime/status?name=  -> {success, data:{name, ready, size_mb, tier}}
//	POST /api/runtime/install       -> {name} triggers EnsureSharedRuntime async;
//	                                   progress arrives via the "runtime" SSE event.
func RegisterRuntimeEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/api/runtime/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		name := r.URL.Query().Get("name")
		dep, ok := SharedRuntimeInfo(name)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "runtime.unknown"})
			return
		}
		_, ready := SharedRuntimePath(name)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"name": name, "ready": ready, "size_mb": dep.SizeMB, "tier": dep.TierRequired,
			},
		})
	})

	mux.HandleFunc("/api/runtime/install", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "http.method_not_allowed"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "" {
			body.Name = r.URL.Query().Get("name")
		}
		if _, ok := SharedRuntimeInfo(body.Name); !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "runtime.unknown"})
			return
		}
		// Fire-and-forget; the UI watches the "runtime" SSE event for progress.
		go func(n string) {
			if _, err := EnsureSharedRuntime(context.Background(), n); err != nil {
				Log.Warn("runtime.install_failed", "name", n, "error", err)
			}
		}(body.Name)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"name": body.Name, "started": true}})
	})
}

// ── download + extract helpers (stdlib-only) ─────────────────────────────────

func rtDownload(ctx context.Context, name, url, tmpDir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	f, err := os.CreateTemp(tmpDir, name+"-dl-*")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	tmpPath := f.Name()
	pw := &rtProgressWriter{name: name, total: resp.ContentLength, last: time.Now()}
	if _, err := io.Copy(io.MultiWriter(f, pw), resp.Body); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return "", fmt.Errorf("write archive: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return "", fmt.Errorf("close archive: %w", err)
	}
	return tmpPath, nil
}

type rtProgressWriter struct {
	name  string
	total int64
	seen  int64
	last  time.Time
}

func (p *rtProgressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.seen += int64(n)
	if time.Since(p.last) >= 500*time.Millisecond {
		p.last = time.Now()
		pct := 0
		if p.total > 0 {
			pct = int(p.seen * 100 / p.total)
		}
		rtEmit(p.name, "download", pct)
	}
	return n, nil
}

func rtExtract(archivePath, kind, dest string) error {
	switch kind {
	case "zip":
		return rtExtractZip(archivePath, dest)
	case "tar.gz", "tgz", "tar.gz-tree":
		return rtExtractTarGz(archivePath, dest)
	case "tar.xz":
		return fmt.Errorf("tar.xz archives are not supported (choose a .zip or .tar.gz asset)")
	default:
		return fmt.Errorf("unsupported archive format %q", kind)
	}
}

func rtInferArchive(url string) string {
	l := strings.ToLower(url)
	switch {
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(l, ".tar.xz"):
		return "tar.xz"
	default:
		return ""
	}
}

func rtExtractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		target, err := rtSafeJoin(dest, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode()|0o600)
		if err != nil {
			rc.Close()
			return err
		}
		_, cErr := io.Copy(out, rc)
		out.Close()
		rc.Close()
		if cErr != nil {
			return cErr
		}
	}
	return nil
}

func rtExtractTarGz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := rtSafeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // local extraction
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

func rtSafeJoin(dest, name string) (string, error) {
	target := filepath.Join(dest, name)
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) && target != filepath.Clean(dest) {
		return "", fmt.Errorf("illegal path in archive: %q", name)
	}
	return target, nil
}

func rtLocateBinary(root, inner string) (string, error) {
	if inner != "" {
		p := filepath.Join(root, filepath.FromSlash(inner))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	base := filepath.Base(filepath.FromSlash(inner))
	var found string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if info.Name() == base {
			found = path
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("binary %q not found in archive", inner)
	}
	return found, nil
}

func rtMoveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func rtEmit(name, phase string, pct int) {
	PublishEvent("runtime", map[string]any{"name": name, "phase": phase, "pct": pct})
}
