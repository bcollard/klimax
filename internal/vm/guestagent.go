package vm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
)

// limaModuleVersion returns the version of github.com/lima-vm/lima/v2 that
// this binary was compiled against, taken from the embedded build info.
// This ensures the downloaded guest agent always matches the host agent.
// Falls back to a hardcoded value only when build info is unavailable (e.g.
// built with -buildvcs=false or in some test environments).
func limaModuleVersion() string {
	const fallback = "v2.1.0"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return fallback
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/lima-vm/lima/v2" {
			v := dep.Version
			if dep.Replace != nil {
				v = dep.Replace.Version
			}
			if v == "" || v == "(devel)" {
				return fallback
			}
			return v
		}
	}
	return fallback
}

// EnsureGuestAgent returns the path to the cached lima guest agent binary for
// the current host architecture, downloading it from Lima's GitHub release if
// not already present.
//
// Cache location: <klimaxHome>/share/lima/lima-guestagent.Linux-<arch>.gz
func EnsureGuestAgent(ctx context.Context, klimaxHome string) (string, error) {
	guestArch := "aarch64"
	if runtime.GOARCH == "amd64" {
		guestArch = "x86_64"
	}

	cacheDir := filepath.Join(klimaxHome, "share", "lima")
	cached := filepath.Join(cacheDir, "lima-guestagent.Linux-"+guestArch+".gz")
	if _, err := os.Stat(cached); err == nil {
		slog.Debug("Lima guest agent already cached", "path", cached)
		return cached, nil
	}

	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return "", fmt.Errorf("creating guest agent cache dir: %w", err)
	}

	// The Darwin release tarball bundles the matching Linux guest agent.
	// Lima names assets using uname -m convention:
	//   arm64  Mac → lima-<ver>-Darwin-arm64.tar.gz  (contains Linux-aarch64 guest agent)
	//   x86_64 Mac → lima-<ver>-Darwin-x86_64.tar.gz (contains Linux-x86_64 guest agent)
	hostArch := "arm64"
	if runtime.GOARCH == "amd64" {
		hostArch = "x86_64"
	}
	limaVer := limaModuleVersion()
	ver := strings.TrimPrefix(limaVer, "v")
	url := fmt.Sprintf(
		"https://github.com/lima-vm/lima/releases/download/%s/lima-%s-Darwin-%s.tar.gz",
		limaVer, ver, hostArch,
	)

	slog.Info("Downloading Lima guest agent", "version", limaVer, "url", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading Lima release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading Lima release: HTTP %s", resp.Status)
	}

	want := "lima-guestagent.Linux-" + guestArch + ".gz"
	if err := extractFromTarGz(resp.Body, want, cached); err != nil {
		return "", fmt.Errorf("extracting %s from Lima release: %w", want, err)
	}

	slog.Info("Lima guest agent cached", "path", cached)
	return cached, nil
}

// extractFromTarGz reads a .tar.gz stream and writes the first entry whose
// base name matches target to destPath.
func extractFromTarGz(r io.Reader, target, destPath string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("reading gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		if filepath.Base(hdr.Name) != target {
			continue
		}
		// Found — write to dest atomically via a temp file.
		tmp := destPath + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("creating temp file: %w", err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("writing guest agent: %w", err)
		}
		if err := f.Close(); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("closing temp file: %w", err)
		}
		return os.Rename(tmp, destPath)
	}
	return fmt.Errorf("%s not found in Lima release tarball", target)
}
