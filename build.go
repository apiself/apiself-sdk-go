package sdk

// Generic build-from-source engine for shared runtimes.
//
// Some dependencies have no prebuilt asset on a given platform (e.g.
// whisper.cpp on macOS ships only an xcframework, no CLI). For those the box
// config declares a Build recipe per platform and this engine produces the
// binary: fetch source (git shallow clone or source tarball), run a recipe
// (cmake / shell), then install the output binary + any sibling libs into the
// shared cache dir. It is recipe-driven and dep-agnostic - it can build
// anything, not just whisper. The caching / locking / shared-location plumbing
// is EnsureSharedRuntimeDep's; this file only "produces the bytes".
//
// The build toolchain (CMake) is itself provisioned as a shared runtime via
// EnsureSharedRuntimeDep - self-hosting, deduped across boxes. A system cmake
// on PATH is preferred when present. A C/C++ compiler is assumed available
// (Xcode CLI tools on macOS, gcc/clang on Linux, MSVC on Windows).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// rtCMakeVersion is the portable CMake fetched from kitware/CMake when no
// system cmake is present. Bumped when a recipe needs a newer minimum.
const rtCMakeVersion = "3.30.5"

// rtBuildHash is a short stable hash of a build recipe so two different build
// configs (flags, target, source ref) of the same version get distinct cache
// dirs and never collide.
func rtBuildHash(b BoxConfigExternalBuild) string {
	data, _ := json.Marshal(b)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:8]
}

// rtBuild builds a dependency from source and installs the output binary
// (plus any sibling libs) flat into cacheDir. Everything happens under tmpDir.
func rtBuild(ctx context.Context, name string, b BoxConfigExternalBuild, cacheDir, tmpDir string, progress RuntimeProgressFn) error {
	if b.Output == "" || b.Binary == "" {
		return fmt.Errorf("build recipe: output and binary are required")
	}
	progress("build", 0)

	srcDir := filepath.Join(tmpDir, "src")
	if err := rtFetchSource(ctx, b.Source, srcDir, tmpDir); err != nil {
		return fmt.Errorf("fetch source: %w", err)
	}

	switch b.Recipe {
	case "cmake":
		if err := rtBuildCMake(ctx, b, srcDir, progress); err != nil {
			return err
		}
	case "shell", "make":
		if err := rtBuildShell(ctx, b, srcDir); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown build recipe %q (want cmake|shell) - add it to sdk build.go rtBuild", b.Recipe)
	}

	if err := rtInstallBuildArtifacts(srcDir, b, cacheDir); err != nil {
		return fmt.Errorf("install artifacts: %w", err)
	}
	return nil
}

// rtFetchSource populates srcDir from a git repo (shallow clone at Ref) or a
// source tarball URL (extracted, single top-level dir stripped if present).
func rtFetchSource(ctx context.Context, src BoxConfigBuildSource, srcDir, tmpDir string) error {
	switch {
	case src.Git != "":
		if _, err := exec.LookPath("git"); err != nil {
			return fmt.Errorf("git not found on PATH (needed to fetch build source)")
		}
		args := []string{"clone", "--depth", "1"}
		if src.Ref != "" {
			args = append(args, "--branch", src.Ref)
		}
		args = append(args, src.Git, srcDir)
		cmd := exec.CommandContext(ctx, "git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone: %v: %s", err, rtTail(out))
		}
		return nil
	case src.URL != "":
		archivePath, err := rtDownload(ctx, src.URL, tmpDir, func(string, int) {})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(srcDir, 0o755); err != nil {
			return err
		}
		kind := strings.TrimSuffix(rtInferArchive(src.URL), "-tree")
		if err := rtExtract(archivePath, kind, srcDir); err != nil {
			return err
		}
		return rtStripSingleTopDir(srcDir)
	default:
		return fmt.Errorf("build source: neither git nor url set")
	}
}

// rtBuildCMake runs a cmake configure + build in srcDir. Defaults the build
// type to Release unless the recipe already sets one.
func rtBuildCMake(ctx context.Context, b BoxConfigExternalBuild, srcDir string, progress RuntimeProgressFn) error {
	cmakeBin, err := rtEnsureCMake(ctx, progress)
	if err != nil {
		return fmt.Errorf("cmake toolchain: %w", err)
	}

	args := []string{"-B", "build"}
	hasBuildType := false
	for _, f := range b.ConfigureFlags {
		if strings.HasPrefix(f, "-DCMAKE_BUILD_TYPE") {
			hasBuildType = true
		}
	}
	if !hasBuildType {
		args = append(args, "-DCMAKE_BUILD_TYPE=Release")
	}
	args = append(args, b.ConfigureFlags...)
	cfg := exec.CommandContext(ctx, cmakeBin, args...)
	cfg.Dir = srcDir
	cfg.Env = rtBuildEnv(b.Env)
	if out, err := cfg.CombinedOutput(); err != nil {
		return fmt.Errorf("cmake configure: %v: %s", err, rtTail(out))
	}

	buildArgs := []string{"--build", "build", "-j", strconv.Itoa(runtime.NumCPU())}
	if b.Target != "" {
		buildArgs = append(buildArgs, "--target", b.Target)
	}
	bld := exec.CommandContext(ctx, cmakeBin, buildArgs...)
	bld.Dir = srcDir
	bld.Env = rtBuildEnv(b.Env)
	return rtRunWithPctProgress(bld, progress)
}

// rtBuildShell runs each recipe command sequentially in srcDir.
func rtBuildShell(ctx context.Context, b BoxConfigExternalBuild, srcDir string) error {
	for _, c := range b.Commands {
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", c)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", c)
		}
		cmd.Dir = srcDir
		cmd.Env = rtBuildEnv(b.Env)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("command %q: %v: %s", c, err, rtTail(out))
		}
	}
	return nil
}

// rtInstallBuildArtifacts copies the primary output binary (renamed to
// Binary's basename) plus any Libs glob matches flat into cacheDir, so
// loader-relative (@rpath / $ORIGIN) sibling libs resolve.
func rtInstallBuildArtifacts(srcDir string, b BoxConfigExternalBuild, cacheDir string) error {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(srcDir, filepath.FromSlash(b.Output))
	dst := filepath.Join(cacheDir, filepath.Base(b.Binary))
	if err := rtCopyFile(out, dst, 0o755); err != nil {
		return fmt.Errorf("copy output %q: %w", b.Output, err)
	}
	if len(b.Libs) == 0 {
		return nil
	}
	// Libs are glob patterns; match on basename (strip any leading **/ dirs).
	pats := make([]string, len(b.Libs))
	for i, p := range b.Libs {
		pats[i] = filepath.Base(filepath.FromSlash(p))
	}
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		for _, pat := range pats {
			if ok, _ := filepath.Match(pat, info.Name()); ok {
				_ = rtCopyFile(path, filepath.Join(cacheDir, info.Name()), 0o644)
				break
			}
		}
		return nil
	})
}

// rtEnsureCMake returns a cmake binary: a system install when present, else a
// portable CMake provisioned as its own shared runtime (self-hosting).
func rtEnsureCMake(ctx context.Context, progress RuntimeProgressFn) (string, error) {
	if p, err := exec.LookPath("cmake"); err == nil {
		return p, nil
	}
	v := rtCMakeVersion
	base := "https://github.com/Kitware/CMake/releases/download/v" + v
	key := platformKey()
	var dl BoxConfigExternalDownload
	switch key {
	case "darwin-amd64", "darwin-arm64":
		dl = BoxConfigExternalDownload{URL: base + "/cmake-" + v + "-macos-universal.tar.gz",
			Archive: "tar.gz-tree", Binary: "cmake-" + v + "-macos-universal/CMake.app/Contents/bin/cmake"}
	case "linux-amd64":
		dl = BoxConfigExternalDownload{URL: base + "/cmake-" + v + "-linux-x86_64.tar.gz",
			Archive: "tar.gz-tree", Binary: "cmake-" + v + "-linux-x86_64/bin/cmake"}
	case "linux-arm64":
		dl = BoxConfigExternalDownload{URL: base + "/cmake-" + v + "-linux-aarch64.tar.gz",
			Archive: "tar.gz-tree", Binary: "cmake-" + v + "-linux-aarch64/bin/cmake"}
	case "windows-amd64":
		dl = BoxConfigExternalDownload{URL: base + "/cmake-" + v + "-windows-x86_64.zip",
			Archive: "zip-tree", Binary: "cmake-" + v + "-windows-x86_64/bin/cmake.exe"}
	default:
		return "", fmt.Errorf("no portable cmake for platform %s (install cmake)", key)
	}
	dep := BoxConfigExternalDep{Name: "cmake", Version: v,
		Downloads: map[string]BoxConfigExternalDownload{key: dl}}
	// Forward only intermediate ticks as "build" progress; swallow cmake's own
	// terminal "ready"/"error" so it doesn't look like the whole dep finished.
	return EnsureSharedRuntimeDep(ctx, dep, func(phase string, pct int) {
		if phase == "download" || phase == "extract" {
			progress("build", 0)
		}
	})
}

// ── helpers ──────────────────────────────────────────────────────────────────

func rtBuildEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

var rtPctRe = regexp.MustCompile(`\[\s*(\d+)%\]`)

// rtRunWithPctProgress runs cmd, forwarding cmake/make "[ NN%]" markers as
// build progress. Output is otherwise discarded (kept only for the error tail).
func rtRunWithPctProgress(cmd *exec.Cmd, progress RuntimeProgressFn) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	var tail strings.Builder
	buf := make([]byte, 8192)
	var carry []byte
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			carry = append(carry, buf[:n]...)
			for {
				i := strings.IndexByte(string(carry), '\n')
				if i < 0 {
					break
				}
				line := string(carry[:i])
				carry = carry[i+1:]
				if m := rtPctRe.FindStringSubmatch(line); m != nil {
					if pct, e := strconv.Atoi(m[1]); e == nil {
						progress("build", pct)
					}
				}
				if tail.Len() < 4096 {
					tail.WriteString(line + "\n")
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("build: %v: %s", err, rtTailStr(tail.String()))
	}
	return nil
}

func rtCopyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// rtStripSingleTopDir collapses a single top-level directory (the common
// "project-1.2.3/" wrapper in source tarballs) so srcDir holds the tree root.
func rtStripSingleTopDir(srcDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return nil
	}
	top := filepath.Join(srcDir, entries[0].Name())
	inner, err := os.ReadDir(top)
	if err != nil {
		return err
	}
	for _, e := range inner {
		if err := os.Rename(filepath.Join(top, e.Name()), filepath.Join(srcDir, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(top)
}

func rtTail(b []byte) string { return rtTailStr(string(b)) }

// rtTailStr returns the last ~600 bytes of build output for error messages.
func rtTailStr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		return "..." + s[len(s)-600:]
	}
	return s
}
