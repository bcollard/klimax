package localdns

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bcollard/marina/internal/config"
	"github.com/bcollard/marina/internal/hostsudo"
)

// ResolverDir is where macOS looks for per-domain resolver files (man 5
// resolver). A file named after a domain sends every query under that domain
// to the nameserver it lists; everything else is untouched.
const ResolverDir = "/etc/resolver"

// resolverMarker is the first line of every file marina writes, so removal
// never touches a resolver file someone else put there.
const resolverMarker = "# Managed by marina"

// legacyResolverMarker is what klimax (marina before v1.0) wrote. Its files
// are cleaned up like marina's own: after a migration, /etc/resolver/klimax.internal
// would otherwise keep pointing macOS at a zone nothing serves.
const legacyResolverMarker = "# Managed by klimax"

// ResolverPath is the resolver file for the configured zone.
func ResolverPath(cfg *config.Config) string {
	return filepath.Join(ResolverDir, cfg.DNSDomain())
}

// ResolverContent is what the file should say. The nameserver is the DNS
// container's address on the kind network, which the host route reaches — so
// unlike an address on lima0, it never changes and the file is written once.
func ResolverContent(cfg *config.Config) string {
	return fmt.Sprintf("%s — local DNS for LoadBalancer Services (network.dns)\nnameserver %s\n", resolverMarker, cfg.DNSServerIP())
}

// HostResolverOK reports whether the resolver file is present and current.
// Unprivileged: /etc/resolver files are world-readable.
func HostResolverOK(cfg *config.Config) bool {
	have, err := os.ReadFile(ResolverPath(cfg))
	return err == nil && string(have) == ResolverContent(cfg)
}

// EnsureHostResolver reconciles /etc/resolver with network.dns: writes the
// zone's file when enabled, and removes any marina-written file that no longer
// matches (the feature turned off, or the domain changed).
//
// Needs root, and only when something changes: a current file is left alone
// without invoking sudo, so re-running `marina up` never prompts for it.
//
// interactive=false (launchd, CI) uses `sudo -n`: it succeeds with cached
// credentials or a NOPASSWD rule and otherwise fails fast with a message,
// instead of hanging on a password prompt nobody can answer. There is
// deliberately no sudoers rule for this in `marina sudoers` — a NOPASSWD tee
// on /etc/resolver would let anything on the Mac redirect any domain.
func EnsureHostResolver(cfg *config.Config, interactive bool) error {
	want := ""
	if cfg.DNSEnabled() {
		want = ResolverPath(cfg)
	}

	for _, stale := range staleResolverFiles(want) {
		slog.Info("Removing marina resolver file", "path", stale)
		if err := hostsudo.Run(interactive, nil, "/bin/rm", "-f", stale); err != nil {
			return fmt.Errorf("removing %s: %w", stale, err)
		}
	}

	if want == "" || HostResolverOK(cfg) {
		return nil
	}
	slog.Info("Writing macOS resolver file (needs sudo, once)", "path", want, "nameserver", cfg.DNSServerIP())
	if err := hostsudo.Run(interactive, nil, "/bin/mkdir", "-p", ResolverDir); err != nil {
		return err
	}
	if err := hostsudo.Run(interactive, strings.NewReader(ResolverContent(cfg)), "/usr/bin/tee", want); err != nil {
		return err
	}
	return nil
}

// RemoveHostResolvers deletes every resolver file marina wrote. Used by
// `marina destroy`.
func RemoveHostResolvers(interactive bool) error {
	for _, p := range staleResolverFiles("") {
		if err := hostsudo.Run(interactive, nil, "/bin/rm", "-f", p); err != nil {
			return err
		}
	}
	return nil
}

// staleResolverFiles lists marina-written resolver files other than keep.
func staleResolverFiles(keep string) []string {
	entries, err := os.ReadDir(ResolverDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(ResolverDir, e.Name())
		if p == keep || e.IsDir() {
			continue
		}
		b, err := os.ReadFile(p)
		if err == nil && (strings.HasPrefix(string(b), resolverMarker) || strings.HasPrefix(string(b), legacyResolverMarker)) {
			out = append(out, p)
		}
	}
	return out
}
