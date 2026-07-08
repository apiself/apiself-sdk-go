package sdk

// Box-side AI model downloader.
//
// Phase 4 refactor (2026-06-09): boxes own their model catalogues and
// stream model files directly to the shared on-disk cache without
// routing through the manager. The shared layout is preserved -
// {DataDir}/shared/models/<family>/<id>.<ext> - so a model installed
// by the LLM box is reused by a future image-gen box without re-download.
//
// Why bypass the manager:
//   - Boxes work even when the manager is offline. The box UI can list
//     installed models, run synthesis / chat / transcription, and pull
//     new models on demand. The manager remains the box-lifecycle
//     orchestrator (spawn / proxy / cross-box routing) but is no longer
//     a single point of failure for AI work.
//   - The manager binary stays free of per-family knowledge. No more
//     manifests/<family>.json embedded in the manager; each box ships
//     its own preset list in config.json.
//
// Inter-box safety:
//   - File lock at <dest>.lock during the download window. Concurrent
//     EnsureModel(...) calls from sibling boxes either reuse the
//     in-flight download (poll for the .ok sentinel) or race-skip if
//     the file already exists when the second caller arrives.
//   - Companion files (e.g. piper's .onnx.json sitting next to the
//     .onnx) are fetched after the primary file lands, in the same
//     directory.
//   - `.ok` sentinel marks a complete extract; presence means cache hit.
//     A crashed process leaves a `<dest>.lock` file behind which we
//     treat as stale after 10 minutes and re-acquire.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnsureModel makes sure the model at the given URL is on disk and
// returns its absolute path. Idempotent: cache hit returns immediately.
// First caller does the download; concurrent callers (other boxes,
// other goroutines) poll the lock until the file lands.
//
// Layout (matches the legacy Manager path so existing installs aren't
// re-downloaded after this refactor):
//
//	{DataDir}/shared/models/<family>/<id>.<ext>
//	{DataDir}/shared/models/<family>/<id>.<ext>.json     (if companion)
//	{DataDir}/shared/models/<family>/<id>.<ext>.ok       (sentinel)
//	{DataDir}/shared/models/<family>/<id>.<ext>.lock     (in-flight)
//
// Args:
//   - family       : "piper", "whisper", "llm", ...
//   - id           : model id (e.g. "en_US-lessac-medium")
//   - ext          : file extension without dot ("onnx", "bin", "gguf")
//   - url          : direct HTTPS URL to the primary file
//   - companionURLs: optional secondary files (e.g. .onnx.json config)
//                    landed at <dest>.<companion suffix>; see helper
//                    companionDestPath() in this package.
//   - timeout      : overall ceiling for the download; per-byte rate is
//                    not enforced - large files on slow links need a
//                    correspondingly large timeout (30 minutes for
//                    Whisper Large, 5 minutes for a 64 MB Piper voice).
func EnsureModel(family, id, ext, url string, companionURLs []string, timeout time.Duration) (string, error) {
	if family == "" || id == "" || ext == "" || url == "" {
		return "", fmt.Errorf("EnsureModel: family/id/ext/url all required")
	}

	// Phase D.2: resolve huggingface:// pseudo-URLs to canonical HF resolve
	// links before the download path runs. Both the primary URL and any
	// companions go through the same translation so a box can write all
	// of its config.json entries in the short form.
	var err error
	if url, err = resolveHuggingFaceURL(url); err != nil {
		return "", fmt.Errorf("EnsureModel: %w", err)
	}
	resolvedCompanions := make([]string, 0, len(companionURLs))
	for _, cu := range companionURLs {
		r, err := resolveHuggingFaceURL(cu)
		if err != nil {
			return "", fmt.Errorf("EnsureModel companion: %w", err)
		}
		resolvedCompanions = append(resolvedCompanions, r)
	}
	companionURLs = resolvedCompanions

	dataDir := PlatformDataDir()
	if dataDir == "" {
		return "", fmt.Errorf("EnsureModel: cannot resolve data dir for this platform")
	}
	migrateSharedLayoutOnce()
	famDir := filepath.Join(dataDir, "shared", "models", family)
	dest := filepath.Join(famDir, id+"."+ext)
	sentinel := dest + ".ok"
	lockPath := dest + ".lock"

	// Cache hit: sentinel + file present, no work to do.
	if _, err := os.Stat(sentinel); err == nil {
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
	}

	if err := os.MkdirAll(famDir, 0o755); err != nil {
		return "", fmt.Errorf("EnsureModel: mkdir: %w", err)
	}

	// Acquire the lock. A sibling box that's already downloading the
	// same model is detected by O_EXCL failing - in that case we wait
	// for the sentinel instead of duplicating the fetch.
	locked, err := acquireDownloadLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("EnsureModel: lock: %w", err)
	}
	if !locked {
		if err := waitForSentinel(sentinel, timeout); err != nil {
			return "", err
		}
		return dest, nil
	}
	defer os.Remove(lockPath)

	// Re-check the sentinel after acquiring - another process may have
	// completed between our first check and the lock claim.
	if _, err := os.Stat(sentinel); err == nil {
		if _, err := os.Stat(dest); err == nil {
			return dest, nil
		}
	}

	tmp := dest + ".download"
	defer os.Remove(tmp)

	if err := downloadFile(url, tmp, timeout); err != nil {
		return "", fmt.Errorf("EnsureModel: primary download %s: %w", url, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", fmt.Errorf("EnsureModel: rename: %w", err)
	}

	// Companions: each lands at the path the manager-side runAIModelDownload
	// derived - if the companion URL ends with "<id>.<ext>.<suffix>" we
	// drop the "<id>.<ext>" prefix and re-anchor on the local primary
	// path. Otherwise the companion's basename is used verbatim alongside
	// the primary.
	primaryBase := filepath.Base(url)
	for _, cu := range companionURLs {
		cDest := companionDestPath(primaryBase, dest, cu)
		cTmp := cDest + ".download"
		if err := downloadFile(cu, cTmp, timeout); err != nil {
			_ = os.Remove(cTmp)
			_ = os.Remove(dest)
			return "", fmt.Errorf("EnsureModel: companion %s: %w", filepath.Base(cu), err)
		}
		if err := os.Rename(cTmp, cDest); err != nil {
			_ = os.Remove(cTmp)
			_ = os.Remove(dest)
			return "", fmt.Errorf("EnsureModel: companion rename: %w", err)
		}
	}

	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		// Sentinel write failure isn't fatal - cache hit just won't
		// short-circuit next time and we'll re-extract. Log and move on.
		// (Logger not always wired this deep in the SDK; swallow.)
		_ = err
	}
	return dest, nil
}

// acquireDownloadLock atomically creates the lock file. Returns true
// when this caller is the writer; false when someone else holds it.
// Stale locks (>10 min old, suggesting a crashed downloader) are forced
// open since their original holder isn't coming back.
func acquireDownloadLock(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err == nil {
		f.Close()
		return true, nil
	}
	if !os.IsExist(err) {
		return false, err
	}
	// Lock exists - check age. Stale (>10 min) lock means the previous
	// holder died; we steal it.
	if info, statErr := os.Stat(path); statErr == nil {
		if time.Since(info.ModTime()) > 10*time.Minute {
			_ = os.Remove(path)
			// Try once more after the stale-lock cleanup.
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err == nil {
				f.Close()
				return true, nil
			}
		}
	}
	return false, nil
}

// waitForSentinel polls for the sentinel file. Used by the second-
// caller path when a sibling already has the lock. Polls every second
// up to the caller-supplied timeout.
func waitForSentinel(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for sibling download (%s)", path)
}

// companionDestPath maps a companion URL onto a path next to the primary
// file. When the companion's basename starts with the primary's basename
// (the common piper case: foo.onnx + foo.onnx.json), we lift the trailing
// suffix onto the local dest. Otherwise the companion lands at
// <dest dir>/<companion basename>.
func companionDestPath(primaryBase, dest, companionURL string) string {
	companionBase := filepath.Base(companionURL)
	if primaryBase != "" && len(companionBase) > len(primaryBase) {
		if strings.HasPrefix(companionBase, primaryBase) {
			return dest + companionBase[len(primaryBase):]
		}
	}
	return filepath.Join(filepath.Dir(dest), companionBase)
}

// resolveHuggingFaceURL translates `huggingface://<owner>/<repo>[@<revision>]/<path>`
// pseudo-URLs to the canonical `https://huggingface.co/<owner>/<repo>/resolve/<revision>/<path>`
// form. Non-`huggingface://` URLs pass through unchanged so callers can mix
// raw HTTPS and HF shorthand freely in the same config.json.
//
// Phase D.2 (2026-06-11, docs/box-dependency-architecture.md §8). Motivation:
// pinning revisions inline ("foo@v1") is far easier to read than the
// canonical "/resolve/v1/" segment buried mid-URL, and a future Phase
// (cn HF mirror, BYO endpoint, paid HF token) can rewrite the resolver
// without touching any box config.
//
// Syntax:
//
//	huggingface://SWivid/F5-TTS/F5TTS_Base/model.safetensors
//	  → https://huggingface.co/SWivid/F5-TTS/resolve/main/F5TTS_Base/model.safetensors
//
//	huggingface://rhasspy/piper-voices@v1.0.0/en/en_US/lessac/medium/foo.onnx
//	  → https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/lessac/medium/foo.onnx
//
// Revision defaults to "main" when no `@<rev>` suffix is present, matching
// the huggingface_hub library default. Owner/repo/path are all required.
func resolveHuggingFaceURL(url string) (string, error) {
	const prefix = "huggingface://"
	if !strings.HasPrefix(url, prefix) {
		return url, nil
	}
	rest := strings.TrimPrefix(url, prefix)
	// Split into owner / (repo[@revision]) / path  — exactly 3 segments.
	// Using SplitN keeps slashes inside `path` intact (model files often
	// sit several levels deep inside HF repos).
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("invalid huggingface:// URL %q: expected owner/repo[@revision]/path", url)
	}
	owner := parts[0]
	repoAndRev := parts[1]
	path := parts[2]
	revision := "main"
	if at := strings.Index(repoAndRev, "@"); at >= 0 {
		revision = repoAndRev[at+1:]
		repoAndRev = repoAndRev[:at]
		if revision == "" {
			return "", fmt.Errorf("invalid huggingface:// URL %q: empty revision after @", url)
		}
	}
	if repoAndRev == "" {
		return "", fmt.Errorf("invalid huggingface:// URL %q: empty repo", url)
	}
	return fmt.Sprintf("https://huggingface.co/%s/%s/resolve/%s/%s",
		owner, repoAndRev, revision, path), nil
}

// downloadFile streams URL to dest. No retry, no resume - one shot.
// Callers wrap with a timeout (HTTP client + ctx) sized to the expected
// file. Used by EnsureModel and its companion helper.
func downloadFile(url, dest string, timeout time.Duration) error {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "apiself-sdk-go/1.0 (model-fetch)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d for %s", resp.StatusCode, url)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}
