package sdk

// One-time migration of the {DataDir}/shared/ tree to the tidy, type-grouped
// layout (2026-07):
//
//	shared/runtime/<name>/<version>/   engines & binaries (ffmpeg, piper, …)
//	shared/models/<family>/<id>.<ext>  AI model weights
//	shared/datasets/<name>/            static asset bundles
//	shared/cache/<name>/               transient caches (hf, …)
//	shared/registry/                   (kept as-is)
//
// Legacy flat layout put every runtime directly under shared/ (shared/ffmpeg,
// shared/piper, …), models under shared/ai-models, and the HF cache under
// shared/hf-cache. This relocates them once, idempotently and best-effort:
// a locked/failed move is logged and skipped (the box then re-downloads into
// the new location rather than crashing). Runs lazily via
// migrateSharedLayoutOnce() before the first shared/ path is resolved.

import (
	"os"
	"path/filepath"
	"sync"
)

var sharedMigrateOnce sync.Once

func migrateSharedLayoutOnce() { sharedMigrateOnce.Do(func() { _ = MigrateSharedLayout() }) }

// reservedSharedGroup are the top-level shared/ entries that are group dirs or
// have their own layout — never treated as a flat runtime to relocate.
var reservedSharedGroup = map[string]bool{
	"runtime": true, "models": true, "datasets": true, "cache": true,
	"registry": true, "ai-models": true, "hf-cache": true,
}

// MigrateSharedLayout relocates a legacy flat shared/ tree in place. Safe to
// call repeatedly; a no-op once migrated.
func MigrateSharedLayout() error {
	dataDir := PlatformDataDir()
	if dataDir == "" {
		return nil
	}
	return migrateSharedLayoutAt(filepath.Join(dataDir, "shared"))
}

func migrateSharedLayoutAt(root string) error {
	if !isDir(root) {
		return nil
	}

	// 1. models: shared/ai-models -> shared/models
	moveSharedDir(filepath.Join(root, "ai-models"), filepath.Join(root, "models"))

	// 2. hf cache: shared/hf-cache -> shared/cache/hf
	moveSharedDir(filepath.Join(root, "hf-cache"), filepath.Join(root, "cache", "hf"))

	// 3. flat runtimes: shared/<name> -> shared/runtime/<name>
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || reservedSharedGroup[e.Name()] {
			continue
		}
		moveSharedDir(filepath.Join(root, e.Name()), filepath.Join(root, "runtime", e.Name()))
	}
	return nil
}

// moveSharedDir renames src->dst if src exists and dst does not. Best-effort:
// never clobbers an existing target, logs (does not fail) on a locked move.
func moveSharedDir(src, dst string) {
	if !isDir(src) || pathExists(dst) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		Log.Warn("shared.migrate_mkdir_failed", "dst", dst, "err", err.Error())
		return
	}
	if err := os.Rename(src, dst); err != nil {
		Log.Warn("shared.migrate_move_failed", "src", src, "dst", dst, "err", err.Error())
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
