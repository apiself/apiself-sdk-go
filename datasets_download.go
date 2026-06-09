package sdk

// Static dataset downloader. Paired with EnsureModel but for bulk
// asset bundles that ship with the box (preview audio, language data,
// test corpora) rather than per-row downloadable model files.
//
// Layout:
//
//	{DataDir}/shared/datasets/<name>/             (extracted directory)
//	{DataDir}/shared/datasets/<name>.ok           (sentinel)
//	{DataDir}/shared/datasets/<name>.lock         (in-flight)
//	{DataDir}/shared/datasets/<name>.download     (temp archive)
//
// Concurrency safety, file lock, sentinel-driven cache hits, stale
// lock reclaim - all identical to EnsureModel. SHA-256 verified after
// the download lands so a poisoned host cannot ship a payload that
// differs from the one config.json pinned.

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnsureDataset makes sure the asset bundle at the given URL is on
// disk + extracted and returns the directory path. Idempotent: cache
// hit returns immediately. First caller does the download + extract;
// concurrent callers (sibling boxes that share the dataset) poll the
// lock until the sentinel lands.
//
// Args:
//   - name    : stable dataset identifier (e.g. "piper-samples").
//               Becomes the cache directory name. Pick a name that
//               includes a version suffix (`piper-samples-v1`) so a
//               future incompatible bundle can roll out without
//               invalidating the cache.
//   - url     : HTTPS URL to the archive. GitHub Release attachments
//               on the box's mono-repo are the recommended host.
//   - sha256  : hex-encoded SHA-256 of the archive (NOT of the
//               extracted contents). Verified before extract.
//   - timeout : overall ceiling for the download + extract.
//
// The archive format is detected from the URL extension: ".tar.gz",
// ".tgz", ".tar.bz2", ".zip" are supported. When the archive's top
// level is a single directory wrapping all entries, that directory
// is unwrapped so callers see the contents directly under
// `<DataDir>/shared/datasets/<name>/`.
func EnsureDataset(name, url, sha256hex string, timeout time.Duration) (string, error) {
	if name == "" || url == "" {
		return "", fmt.Errorf("EnsureDataset: name and url required")
	}
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return "", fmt.Errorf("EnsureDataset: cannot resolve data dir for this platform")
	}
	root := filepath.Join(dataDir, "shared", "datasets")
	dir := filepath.Join(root, name)
	sentinel := dir + ".ok"
	lockPath := dir + ".lock"
	tmp := dir + ".download"

	// Cache hit.
	if _, err := os.Stat(sentinel); err == nil {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("EnsureDataset: mkdir: %w", err)
	}

	locked, err := acquireDownloadLock(lockPath)
	if err != nil {
		return "", fmt.Errorf("EnsureDataset: lock: %w", err)
	}
	if !locked {
		if err := waitForSentinel(sentinel, timeout); err != nil {
			return "", err
		}
		return dir, nil
	}
	defer os.Remove(lockPath)

	// Re-check sentinel after acquiring the lock.
	if _, err := os.Stat(sentinel); err == nil {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}

	// Download to tmp.
	defer os.Remove(tmp)
	if err := downloadFile(url, tmp, timeout); err != nil {
		return "", fmt.Errorf("EnsureDataset: download %s: %w", url, err)
	}

	// Verify SHA-256 if the caller pinned one. A mismatch is treated as
	// hostile - we delete the temp file and bail without touching the
	// destination directory.
	if sha256hex != "" {
		got, err := fileSHA256(tmp)
		if err != nil {
			return "", fmt.Errorf("EnsureDataset: hash: %w", err)
		}
		if !strings.EqualFold(got, sha256hex) {
			return "", fmt.Errorf("EnsureDataset: sha256 mismatch (got %s, want %s)", got, sha256hex)
		}
	}

	// Extract into a fresh staging directory then atomic-rename onto
	// the final path. If a previous incomplete attempt left junk under
	// `dir`, the rename swaps cleanly without us having to recursively
	// delete it ourselves.
	staging := dir + ".extracting"
	_ = os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("EnsureDataset: mkdir staging: %w", err)
	}
	if err := extractArchive(tmp, staging, url); err != nil {
		_ = os.RemoveAll(staging)
		return "", fmt.Errorf("EnsureDataset: extract: %w", err)
	}
	// If the archive had a single top-level directory wrapping every
	// entry, unwrap it so callers see the dataset contents directly
	// under `dir`.
	unwrapped := maybeUnwrapTopLevelDir(staging)

	// Replace `dir` atomically.
	_ = os.RemoveAll(dir)
	if err := os.Rename(unwrapped, dir); err != nil {
		// Rename may fail if unwrapped is on a different volume from
		// dir's parent (rare since we built them under the same root,
		// but a network FS or symlink can surprise us). Fall back to a
		// copy + delete.
		if err2 := os.Rename(staging, dir); err2 == nil {
			// fine - staging was the right level already
		} else {
			return "", fmt.Errorf("EnsureDataset: rename: %w", err)
		}
	}
	if unwrapped != staging {
		_ = os.RemoveAll(staging)
	}

	if err := os.WriteFile(sentinel, []byte("ok"), 0o644); err != nil {
		// Non-fatal: cache hit just won't short-circuit next launch.
		_ = err
	}
	return dir, nil
}

// DatasetDir returns the on-disk directory path for a dataset. Does
// NOT check if it exists - callers stat actual files to determine
// readiness. Useful for handlers that serve content from the dataset
// without caring about lifecycle.
func DatasetDir(name string) string {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "shared", "datasets", name)
}

// DatasetReady returns true iff the .ok sentinel exists for the dataset.
// Cheap check (one stat) suitable for status endpoints that poll.
func DatasetReady(name string) bool {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return false
	}
	sentinel := filepath.Join(dataDir, "shared", "datasets", name+".ok")
	_, err := os.Stat(sentinel)
	return err == nil
}

// ─── helpers ───────────────────────────────────────────────────────────

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractArchive picks the right decoder based on the URL extension
// (since callers pin URLs explicitly the URL is authoritative; we
// avoid magic-byte sniffing). Extracts into destDir.
func extractArchive(archivePath, destDir, url string) error {
	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archivePath, destDir)
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return extractTarBz2(archivePath, destDir)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, destDir)
	}
	return fmt.Errorf("unsupported archive extension (URL must end in .tar.gz/.tgz/.tar.bz2/.zip): %s", url)
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	return extractTar(tar.NewReader(gz), destDir)
}

func extractTarBz2(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	return extractTar(tar.NewReader(bzip2.NewReader(f)), destDir)
}

func extractTar(tr *tar.Reader, destDir string) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(clean, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		default:
			// Symlinks / devices / fifos: skip silently. Datasets
			// shouldn't ship anything else.
		}
	}
}

func extractZip(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		clean, err := safeJoin(destDir, f.Name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(clean, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// safeJoin ensures the joined path lives under root (zip-slip /
// tar-slip mitigation). Returns an error on entries that try to
// escape.
func safeJoin(root, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(root, name))
	rootClean := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(clean+string(os.PathSeparator), rootClean) && clean != filepath.Clean(root) {
		return "", fmt.Errorf("archive entry escapes destination: %s", name)
	}
	return clean, nil
}

// maybeUnwrapTopLevelDir checks whether `staging` contains exactly one
// child entry which is itself a directory. If so, returns that
// directory's path - callers rename it into place as the dataset
// directory. Otherwise returns `staging` unchanged.
//
// This handles the common case where `tar -czf foo.tar.gz somefolder/`
// produces an archive with `somefolder/` at the top, and we want the
// dataset directory to expose the contents of `somefolder/` directly
// (not nested under it).
func maybeUnwrapTopLevelDir(staging string) string {
	entries, err := os.ReadDir(staging)
	if err != nil || len(entries) != 1 {
		return staging
	}
	if !entries[0].IsDir() {
		return staging
	}
	return filepath.Join(staging, entries[0].Name())
}
