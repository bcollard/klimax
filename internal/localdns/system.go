package localdns

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/bcollard/marina/internal/config"
	"github.com/bcollard/marina/internal/guest"
)

// ProbeLabel names the record marina publishes at the zone apex so the Mac's
// system resolver can be tested with no cluster running. It answers with the
// DNS server's own address, which is in every config and routable from the Mac.
const ProbeLabel = "marina-probe"

// ProbeName is the fully qualified probe name for the configured zone.
func ProbeName(cfg *config.Config) string { return ProbeLabel + "." + cfg.DNSDomain() }

// probeKey stores the probe under an id segment, like ExternalDNS's records,
// so keyToName and every prefix purge treat it the same way.
func probeKey(cfg *config.Config) string { return zoneKey(ProbeName(cfg)) + "marina" }

// EnsureProbeRecord writes the probe record. etcd can take a moment to accept
// writes after its container starts, hence the retry.
func EnsureProbeRecord(ctx context.Context, g *guest.Client, cfg *config.Config) error {
	val := fmt.Sprintf(`{"host":%q,"ttl":5}`, cfg.DNSServerIP())
	_, err := g.Run(ctx, fmt.Sprintf(
		"for i in $(seq 1 20); do docker exec %s etcdctl put %s %s >/dev/null 2>&1 && exit 0; sleep 0.5; done; exit 1",
		EtcdContainer, shellQuote(probeKey(cfg)), shellQuote(val)))
	if err != nil {
		return fmt.Errorf("writing the %s record: %w", ProbeName(cfg), err)
	}
	return nil
}

// LookupViaSystem resolves the probe name the way curl, Chrome and every other
// Mac app do: through the system resolver (getaddrinfo → mDNSResponder), which
// is what honours /etc/resolver. marina builds with cgo, so Go's default
// resolver on darwin is getaddrinfo. ProbeFromHost cannot see a problem on this
// path: it sends its query straight to the server, as `dig @server` does.
//
// It returns the addresses found, or the lookup error.
func LookupViaSystem(ctx context.Context, cfg *config.Config) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return net.DefaultResolver.LookupHost(ctx, ProbeName(cfg))
}

// ManagedDNSProfile describes an MDM-pushed encrypted DNS profile
// (com.apple.dnsSettings.managed). With one installed, mDNSResponder sends
// lookups to the profile's DoH/DoT server instead of following /etc/resolver,
// unless the profile's OnDemandRules exempt the domain.
type ManagedDNSProfile struct {
	Path     string `json:"path"               yaml:"path"`
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Server   string `json:"server,omitempty"   yaml:"server,omitempty"`
}

// managedPrefsDir is where macOS materialises configuration profile payloads,
// system-wide at the top level and per user in subdirectories. A variable so
// tests can point it at a temporary directory.
var managedPrefsDir = "/Library/Managed Preferences"

const managedDNSPlist = "com.apple.dnsSettings.managed.plist"

// FindManagedDNSProfiles returns the managed encrypted DNS profiles on this
// Mac. A plist that cannot be decoded is still reported, without details:
// its presence is the finding.
func FindManagedDNSProfiles() []ManagedDNSProfile {
	paths := []string{filepath.Join(managedPrefsDir, managedDNSPlist)}
	if m, _ := filepath.Glob(filepath.Join(managedPrefsDir, "*", managedDNSPlist)); m != nil {
		paths = append(paths, m...)
	}
	var found []ManagedDNSProfile
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		prof := ManagedDNSProfile{Path: p}
		if out, err := exec.Command("plutil", "-convert", "json", "-o", "-", p).Output(); err == nil {
			prof.Protocol, prof.Server = parseManagedDNS(out)
		}
		found = append(found, prof)
	}
	return found
}

// parseManagedDNS pulls the protocol and server out of the profile's JSON
// form. The keys are searched for at any depth: Apple documents them inside a
// DNSSettings dictionary, but MDMs differ in how they nest the payload.
func parseManagedDNS(data []byte) (protocol, server string) {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return "", ""
	}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if s, ok := t["DNSProtocol"].(string); ok && protocol == "" {
				protocol = s
			}
			for _, k := range []string{"ServerURL", "ServerName"} {
				if s, ok := t[k].(string); ok && server == "" {
					server = s
				}
			}
			if a, ok := t["ServerAddresses"].([]any); ok && server == "" && len(a) > 0 {
				if s, ok := a[0].(string); ok {
					server = s
				}
			}
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(t[k])
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(root)
	return protocol, server
}
