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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
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

// runtimeCacheDirSeg is {DataDir}/shared/runtime/{name}/{segment}.
// Runtimes live under the shared/runtime/ group (unified 2026-07 to match the
// Manager's RuntimeDir + keep shared/ tidy: runtime/ models/ datasets/ cache/).
// Legacy flat shared/{name}/ installs are relocated by MigrateSharedLayout.
func runtimeCacheDirSeg(name, segment string) (string, error) {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return "", fmt.Errorf("runtime %q: cannot resolve data dir", name)
	}
	migrateSharedLayoutOnce()
	return filepath.Join(dataDir, "shared", "runtime", name, segment), nil
}

// rtArchiveKind returns the effective archive kind for a download (explicit or
// inferred from the URL suffix).
func rtArchiveKind(dl BoxConfigExternalDownload) string {
	if dl.Archive != "" {
		return dl.Archive
	}
	return rtInferArchive(dl.URL)
}

// rtIsTree reports whether a runtime is a multi-file "tree" (its binary needs
// sibling files - DLLs, .pak, icudtl.dat - kept next to it, e.g.
// chrome-headless-shell or a full python runtime). Signalled by a "-tree"
// archive-kind suffix (zip-tree, tar.gz-tree). Non-tree runtimes are a single
// self-contained binary (pandoc) flattened into the cache dir.
func rtIsTree(dl BoxConfigExternalDownload) bool {
	return strings.HasSuffix(rtArchiveKind(dl), "-tree")
}

// runtimeCachedBin computes the cached binary path for a download. Tree
// runtimes preserve the archive's relative structure so companions stay put;
// single-binary runtimes flatten to {cacheDir}/{basename}.
func runtimeCachedBin(dl BoxConfigExternalDownload, cacheDir string) string {
	if rtIsTree(dl) {
		return filepath.Join(cacheDir, filepath.FromSlash(dl.Binary))
	}
	return filepath.Join(cacheDir, filepath.Base(dl.Binary))
}

// runtimePlan is the resolved install method for the current platform: either
// a prebuilt download or a build-from-source recipe, with the computed cache
// dir + binary path. Build variants get a flags-hash version suffix so two
// different build configs of the same version don't share a cache dir.
type runtimePlan struct {
	cacheDir  string
	cachedBin string
	isBuild   bool
	dl        BoxConfigExternalDownload
	bld       BoxConfigExternalBuild
}

// resolveRuntimePlan picks Downloads[platform] first, else Build[platform].
func resolveRuntimePlan(dep *BoxConfigExternalDep) (runtimePlan, error) {
	key := platformKey()
	if dl, ok := dep.Downloads[key]; ok && dl.URL != "" {
		if dl.Binary == "" {
			return runtimePlan{}, fmt.Errorf("runtime %q: no binary path for %s", dep.Name, key)
		}
		cacheDir, err := runtimeCacheDirSeg(dep.Name, runtimeVersion(dep))
		if err != nil {
			return runtimePlan{}, err
		}
		return runtimePlan{cacheDir: cacheDir, cachedBin: runtimeCachedBin(dl, cacheDir), dl: dl}, nil
	}
	if bld, ok := dep.Build[key]; ok {
		if bld.Binary == "" {
			return runtimePlan{}, fmt.Errorf("runtime %q: build recipe has no binary for %s", dep.Name, key)
		}
		seg := runtimeVersion(dep) + "-" + rtBuildHash(bld)
		cacheDir, err := runtimeCacheDirSeg(dep.Name, seg)
		if err != nil {
			return runtimePlan{}, err
		}
		return runtimePlan{
			cacheDir:  cacheDir,
			cachedBin: filepath.Join(cacheDir, filepath.Base(bld.Binary)),
			isBuild:   true,
			bld:       bld,
		}, nil
	}
	return runtimePlan{}, fmt.Errorf("runtime %q: not available on this platform (%s)", dep.Name, key)
}

// runtimeBinPath resolves the cached binary path for a dep (may not exist yet).
func runtimeBinPath(dep *BoxConfigExternalDep) (string, error) {
	p, err := resolveRuntimePlan(dep)
	if err != nil {
		return "", err
	}
	return p.cachedBin, nil
}

// SharedRuntimePath returns the cached binary path for `name` if it is already
// installed (does not download). ready=false when absent or not on this platform.
func SharedRuntimePath(name string) (string, bool) {
	dep, err := findExternalDep(name)
	if err != nil {
		return "", false
	}
	plan, err := resolveRuntimePlan(dep)
	if err != nil {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(plan.cacheDir, ".ok")); err != nil {
		return "", false
	}
	if fi, err := os.Stat(plan.cachedBin); err != nil || fi.IsDir() {
		return "", false
	}
	return plan.cachedBin, true
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
//
// This is the box-side entry point: it resolves the dep from the box's own
// config.json and reports progress via the "runtime" SSE event. The manager
// (which installs on behalf of a box it doesn't run in) calls
// EnsureSharedRuntimeDep directly with the box's dep + its own progress sink.
func EnsureSharedRuntime(ctx context.Context, name string) (string, error) {
	dep, err := findExternalDep(name)
	if err != nil {
		rtEmit(name, "error", 0)
		return "", err
	}
	return EnsureSharedRuntimeDep(ctx, *dep, func(phase string, pct int) {
		rtEmit(name, phase, pct)
	})
}

// RuntimeProgressFn reports install progress: phase in
// {download, extract, ready, error}, pct 0-100 (meaningful for "download").
type RuntimeProgressFn func(phase string, pct int)

// EnsureSharedRuntimeDep is the single installer both the box (via
// EnsureSharedRuntime) and the manager call. It installs dep's binary into the
// shared data dir ({DataDir}/shared/{name}/{version}/) and returns the absolute
// path to it. Idempotent (cache hit returns immediately), cross-process safe,
// and never panics. Progress is delivered via the callback so the caller owns
// the transport (box SSE, manager SSE, CLI, ...).
func EnsureSharedRuntimeDep(ctx context.Context, dep BoxConfigExternalDep, progress RuntimeProgressFn) (string, error) {
	if progress == nil {
		progress = func(string, int) {}
	}
	name := dep.Name
	fail := func(err error) (string, error) { progress("error", 0); return "", err }

	plan, err := resolveRuntimePlan(&dep)
	if err != nil {
		return fail(err)
	}
	cacheDir := plan.cacheDir
	cachedBin := plan.cachedBin
	sentinel := filepath.Join(cacheDir, ".ok")

	// Fast path: sentinel + binary present.
	if _, err := os.Stat(sentinel); err == nil {
		if fi, err := os.Stat(cachedBin); err == nil && !fi.IsDir() {
			return cachedBin, nil
		}
	}

	// In-process serialization (this process).
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
		return fail(fmt.Errorf("runtime %q: create cache dir: %w", name, err))
	}

	// Cross-process lock (other boxes / the manager). If a sibling holds it,
	// wait for the sentinel instead of downloading twice.
	lockPath := filepath.Join(cacheDir, ".lock")
	locked, err := acquireDownloadLock(lockPath)
	if err != nil {
		return fail(fmt.Errorf("runtime %q: lock: %w", name, err))
	}
	if !locked {
		if err := waitForSentinel(sentinel, 30*time.Minute); err != nil {
			return fail(err)
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
		return fail(fmt.Errorf("runtime %q: temp dir: %w", name, err))
	}
	defer os.RemoveAll(tmpDir)

	// treeInstall marks installs that lay down a whole directory of files
	// (tree archive OR a build with sibling libs) - chmod the whole tree so
	// helper executables run.
	treeInstall := plan.isBuild

	if plan.isBuild {
		// Build-from-source: clone/fetch, compile, install the output binary
		// plus any sibling libs flat into cacheDir.
		if err := rtBuild(ctx, name, plan.bld, cacheDir, tmpDir, progress); err != nil {
			return fail(fmt.Errorf("runtime %q: build: %w", name, err))
		}
		if fi, err := os.Stat(cachedBin); err != nil || fi.IsDir() {
			return fail(fmt.Errorf("runtime %q: binary %q missing after build", name, plan.bld.Binary))
		}
	} else {
		dl := plan.dl
		binName := dl.Binary
		archiveKind := rtArchiveKind(dl)
		isTree := rtIsTree(dl)
		treeInstall = isTree
		extractKind := strings.TrimSuffix(archiveKind, "-tree")

		progress("download", 0)
		archivePath, err := rtDownload(ctx, dl.URL, tmpDir, progress)
		if err != nil {
			return fail(err)
		}

		switch {
		case archiveKind == "" || archiveKind == "raw" || archiveKind == "binary":
			// The download IS the binary; move it straight in.
			if err := rtMoveFile(archivePath, cachedBin); err != nil {
				return fail(fmt.Errorf("runtime %q: install: %w", name, err))
			}
		case isTree:
			// Multi-file runtime (chrome, python, llama-cpp): extract the whole
			// archive into the cache dir so the binary keeps its sibling libs/data.
			progress("extract", 0)
			if err := rtExtract(archivePath, extractKind, cacheDir); err != nil {
				return fail(fmt.Errorf("runtime %q: extract: %w", name, err))
			}
			if fi, err := os.Stat(cachedBin); err != nil || fi.IsDir() {
				return fail(fmt.Errorf("runtime %q: binary %q missing after extract", name, binName))
			}
		default:
			// Single-file archive (pandoc single binary, libgomp .so from a .deb):
			// extract to temp, pluck the one file, install under the binary's
			// basename. The `inner` field (when set) is the path to locate INSIDE
			// the archive; `binary` is the installed name (SONAME for libgomp).
			progress("extract", 0)
			extractDir, err := os.MkdirTemp(tmpDir, name+"-x-")
			if err != nil {
				return fail(fmt.Errorf("runtime %q: extract dir: %w", name, err))
			}
			if err := rtExtract(archivePath, extractKind, extractDir); err != nil {
				return fail(fmt.Errorf("runtime %q: extract: %w", name, err))
			}
			locate := dl.Inner
			if locate == "" {
				locate = binName
			}
			found, err := rtLocateBinary(extractDir, locate)
			if err != nil {
				return fail(fmt.Errorf("runtime %q: %w", name, err))
			}
			if err := rtMoveFile(found, cachedBin); err != nil {
				return fail(fmt.Errorf("runtime %q: install: %w", name, err))
			}
		}
	}

	if runtime.GOOS != "windows" {
		if treeInstall {
			// The tree may contain helper executables (chrome's crashpad
			// handler, build sibling libs); make regular files runnable.
			rtChmodTree(cacheDir)
		} else {
			_ = os.Chmod(cachedBin, 0o755)
		}
	}
	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		_ = err // non-fatal; next call just re-verifies the binary
	}

	progress("ready", 100)
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

func rtDownload(ctx context.Context, url, tmpDir string, progress RuntimeProgressFn) (string, error) {
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
	f, err := os.CreateTemp(tmpDir, "dl-*")
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	tmpPath := f.Name()
	pw := &rtProgressWriter{progress: progress, total: resp.ContentLength, last: time.Now()}
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
	progress RuntimeProgressFn
	total    int64
	seen     int64
	last     time.Time
	lastPct  int
}

func (p *rtProgressWriter) Write(b []byte) (int, error) {
	n := len(b)
	p.seen += int64(n)
	pct := 0
	if p.total > 0 {
		pct = int(p.seen * 100 / p.total)
	}
	// Emit on a 150ms tick OR whenever the percentage advances - so even a
	// small/fast download (piper ~20 MB) shows visible movement instead of
	// jumping straight from 0% to ready.
	if time.Since(p.last) >= 150*time.Millisecond || pct > p.lastPct {
		p.last = time.Now()
		p.lastPct = pct
		p.progress("download", pct)
	}
	return n, nil
}

func rtExtract(archivePath, kind, dest string) error {
	switch kind {
	case "zip":
		return rtExtractZip(archivePath, dest)
	case "tar.gz", "tgz":
		return rtExtractTarGz(archivePath, dest)
	case "tar.xz":
		return rtExtractTarXz(archivePath, dest)
	case "tar.zst", "tzst":
		return rtExtractTarZst(archivePath, dest)
	case "deb":
		return rtExtractDeb(archivePath, dest)
	default:
		return fmt.Errorf("unsupported archive format %q "+
			"(add an extractor to sdk runtime.go rtExtract)", kind)
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
	case strings.HasSuffix(l, ".tar.zst"), strings.HasSuffix(l, ".tzst"):
		return "tar.zst"
	case strings.HasSuffix(l, ".deb"):
		return "deb"
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
	return rtExtractTarStream(gz, dest)
}

func rtExtractTarXz(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	xr, err := xz.NewReader(f)
	if err != nil {
		return fmt.Errorf("xz: %w", err)
	}
	return rtExtractTarStream(xr, dest)
}

func rtExtractTarZst(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd: %w", err)
	}
	defer zr.Close()
	return rtExtractTarStream(zr, dest)
}

// rtExtractTarStream extracts a tar stream (already decompressed) into dest,
// preserving directory structure and symlinks. Shared by tar.gz / tar.xz / the
// inner data tarball of a .deb.
func rtExtractTarStream(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
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
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
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

// rtExtractDeb extracts a Debian .deb package into dest. A .deb is an `ar`
// archive containing debian-binary, control.tar.*, and data.tar.{gz,xz,zst};
// we extract the data tarball (the actual files) into dest. Used for single
// shared libraries pulled from distro packages (e.g. libgomp).
func rtExtractDeb(src, dest string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	member, r, err := rtArFindMember(f, "data.tar")
	if err != nil {
		return fmt.Errorf("deb: %w", err)
	}
	switch {
	case strings.HasSuffix(member, ".gz"):
		gz, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("deb gzip: %w", err)
		}
		defer gz.Close()
		return rtExtractTarStream(gz, dest)
	case strings.HasSuffix(member, ".xz"):
		xr, err := xz.NewReader(r)
		if err != nil {
			return fmt.Errorf("deb xz: %w", err)
		}
		return rtExtractTarStream(xr, dest)
	case strings.HasSuffix(member, ".zst"):
		zr, err := zstd.NewReader(r)
		if err != nil {
			return fmt.Errorf("deb zstd: %w", err)
		}
		defer zr.Close()
		return rtExtractTarStream(zr, dest)
	default:
		return fmt.Errorf("deb: unsupported data member %q (only .gz/.xz/.zst)", member)
	}
}

// rtArFindMember scans a Unix `ar` archive for the first member whose name
// starts with prefix, returning the full member name and a reader limited to
// its bytes. `ar` format: an 8-byte magic ("!<arch>\n") then, per member, a
// 60-byte header (name[16], mtime[12], uid[6], gid[6], mode[8], size[10],
// magic[2]) followed by size bytes, padded to an even offset.
func rtArFindMember(f *os.File, prefix string) (string, io.Reader, error) {
	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", nil, err
	}
	if string(magic) != "!<arch>\n" {
		return "", nil, fmt.Errorf("not an ar archive")
	}
	hdr := make([]byte, 60)
	for {
		if _, err := io.ReadFull(f, hdr); err != nil {
			if errors.Is(err, io.EOF) {
				return "", nil, fmt.Errorf("member %q* not found", prefix)
			}
			return "", nil, err
		}
		name := strings.TrimRight(string(hdr[0:16]), " ")
		name = strings.TrimSuffix(name, "/") // GNU ar terminates names with '/'
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil {
			return "", nil, fmt.Errorf("ar size: %w", err)
		}
		if strings.HasPrefix(name, prefix) {
			return name, io.LimitReader(f, size), nil
		}
		// Skip this member's data (+1 pad byte when size is odd).
		skip := size
		if size%2 == 1 {
			skip++
		}
		if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
			return "", nil, err
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
	// "runtime" for the per-runtime RuntimeGate/useRuntimeStatus; "dep" is the
	// unified event the <BoxDependencies> card / useBoxDeps listens to
	// (docs/box-dependencies-standard.md). Both carry the same payload.
	PublishEvent("runtime", map[string]any{"name": name, "phase": phase, "pct": pct})
	PublishEvent("dep", map[string]any{"kind": "runtime", "name": name, "phase": phase, "pct": pct})
}

// rtChmodTree makes every regular file in a tree runtime executable (0755) on
// non-Windows so helper binaries next to the main one (crashpad handler, etc.)
// can run. Non-fatal: extraction already succeeded.
func rtChmodTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		_ = os.Chmod(path, 0o755)
		return nil
	})
}
