// Package localdns runs marina's local DNS zone (marina.internal by default):
// an etcd + CoreDNS pair on the kind network that every cluster's ExternalDNS
// writes LoadBalancer Services into, and the Mac resolves through
// /etc/resolver.
//
// The alternative — a real domain plus a cloud DNS provider — is documented on
// the website and is still the only way to get publicly trusted certificates.
// This exists so that a hostname works with no domain, no provider account and
// no per-change sudo.
package localdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bcollard/marina/internal/config"
	"github.com/bcollard/marina/internal/guest"
)

const (
	// ServerContainer serves the zone. It is the only one of the two the Mac
	// can reach: routing.InstallNoNat exempts its address, and only its address,
	// from Docker's direct-routing drop.
	ServerContainer = "marina-dns"
	// EtcdContainer holds the records. Reachable from the kind network only —
	// it accepts unauthenticated writes, so it is not exposed to the host.
	EtcdContainer = "marina-dns-etcd"

	// Pinned rather than "latest": the Corefile and the etcd flags below were
	// validated against exactly these, and a surprise upgrade of either would
	// break name resolution for every cluster at once.
	CoreDNSImage = "coredns/coredns:1.12.4"
	EtcdImage    = "quay.io/coreos/etcd:v3.5.21"

	// EtcdPrefix is where the CoreDNS etcd plugin (SkyDNS layout) and
	// ExternalDNS's coredns provider both look. Names are stored label-reversed
	// under it: web.default.dev.marina.internal →
	// /skydns/internal/marina/dev/default/web/<id>.
	EtcdPrefix = "/skydns"

	corefileDir  = "/etc/marina/dns"
	corefilePath = corefileDir + "/Corefile"

	kindNetwork = "kind"
)

// Corefile renders the CoreDNS config for the zone.
//
// The cache block caps the TTLs clients see, in the authority section too:
//   - success 30: ExternalDNS writes records with TTL 0, which the etcd plugin
//     turns into 300s — a Service recreated on a new VIP would keep resolving
//     to the old one for five minutes on the Mac.
//   - denial 5: the etcd plugin's SOA minimum is a fixed 30s, so a name looked
//     up before ExternalDNS's first sync stayed "no such name" on the Mac for
//     30s after it existed. `rewrite ttl` cannot fix that: it leaves the
//     authority section alone. Measured: a record added after a cached
//     NXDOMAIN is visible within ~6s.
//
// `reload` makes a Corefile edit (a domain change) apply without recreating
// the container.
func Corefile(cfg *config.Config) string {
	return fmt.Sprintf(`# Managed by marina
%s:53 {
    errors
    etcd {
        path %s
        endpoint http://%s:2379
    }
    cache {
        success 9984 30
        denial 9984 5
    }
    reload
}
`, cfg.DNSDomain(), EtcdPrefix, cfg.DNSEtcdIP())
}

// Ensure brings the DNS containers in line with the config: running when
// network.dns is enabled, gone when it is not. Idempotent.
func Ensure(ctx context.Context, g *guest.Client, cfg *config.Config) error {
	if !cfg.DNSEnabled() {
		return Remove(ctx, g)
	}
	slog.Info("Ensuring local DNS", "domain", cfg.DNSDomain(), "server", cfg.DNSServerIP())

	etcdArgs := fmt.Sprintf("etcd --name marina-dns --data-dir /etcd-data"+
		" --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://%s:2379", cfg.DNSEtcdIP())
	if err := ensureContainer(ctx, g, EtcdContainer, EtcdImage, cfg.DNSEtcdIP(), "", etcdArgs); err != nil {
		return err
	}

	current, _ := g.Run(ctx, "sudo cat "+corefilePath+" 2>/dev/null || true")
	want := Corefile(cfg)
	if strings.TrimSpace(current) != strings.TrimSpace(want) {
		if _, err := g.Run(ctx, "sudo mkdir -p "+corefileDir); err != nil {
			return err
		}
		if err := g.WriteFile(ctx, corefilePath, want); err != nil {
			return fmt.Errorf("writing Corefile: %w", err)
		}
	}

	mount := fmt.Sprintf(" -v %s:/etc/coredns:ro", corefileDir)
	return ensureContainer(ctx, g, ServerContainer, CoreDNSImage, cfg.DNSServerIP(), mount, "-conf /etc/coredns/Corefile")
}

// ensureContainer runs one DNS container at a fixed kind-network address,
// recreating it when the image or address it runs with no longer matches.
func ensureContainer(ctx context.Context, g *guest.Client, name, image, ip, extraFlags, args string) error {
	out, err := g.Run(ctx, fmt.Sprintf(
		`docker inspect -f '{{.State.Running}} {{.Config.Image}} {{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' %s 2>/dev/null || true`, name))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == fmt.Sprintf("true %s %s", image, ip) {
		return nil
	}
	if strings.TrimSpace(out) != "" {
		slog.Info("Recreating local DNS container", "name", name, "have", strings.TrimSpace(out))
	}
	if _, err := g.Run(ctx, "docker rm -f "+name+" 2>/dev/null || true"); err != nil {
		return err
	}
	cmd := fmt.Sprintf("docker run -d --restart=always --name %s --network %s --ip %s%s %s %s",
		name, kindNetwork, ip, extraFlags, image, args)
	if _, err := g.Run(ctx, cmd); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	return nil
}

// Remove deletes the DNS containers and their Corefile. The records go with
// the etcd container; they are rebuilt by every cluster's ExternalDNS within
// one sync interval if the feature is turned back on.
func Remove(ctx context.Context, g *guest.Client) error {
	out, err := g.Run(ctx, fmt.Sprintf("docker ps -aq --filter name=^%s$ --filter name=^%s$", ServerContainer, EtcdContainer))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	slog.Info("Removing local DNS (network.dns.enabled is false)")
	_, err = g.Run(ctx, fmt.Sprintf("docker rm -f %s %s >/dev/null 2>&1; sudo rm -rf %s", ServerContainer, EtcdContainer, corefileDir))
	return err
}

// zoneKey is the etcd key prefix holding every record under a DNS name.
func zoneKey(zone string) string {
	labels := strings.Split(strings.TrimSuffix(zone, "."), ".")
	slices.Reverse(labels)
	return EtcdPrefix + "/" + strings.Join(labels, "/") + "/"
}

// PurgeCluster deletes a cluster's records. ExternalDNS dies with its cluster,
// so it never gets to clean up after itself; without this every deleted
// cluster's names would keep resolving to VIPs nobody answers on.
//
// A no-op when the DNS containers are not running.
func PurgeCluster(ctx context.Context, g *guest.Client, cfg *config.Config, cluster string) error {
	key := zoneKey(cfg.ClusterDNSZone(cluster))
	_, err := g.Run(ctx, fmt.Sprintf(
		"docker exec %s etcdctl del --prefix %s >/dev/null 2>&1 || true", EtcdContainer, shellQuote(key)))
	return err
}

// Record is one published name.
type Record struct {
	Name string `json:"name" yaml:"name"`
	IP   string `json:"ip"   yaml:"ip"`
}

// ListRecords returns the A records in the zone, sorted by name. ExternalDNS's
// ownership TXT records are left out: they are bookkeeping, not names anyone
// resolves.
func ListRecords(ctx context.Context, g *guest.Client, cfg *config.Config) ([]Record, error) {
	out, err := g.Run(ctx, fmt.Sprintf(
		"docker exec %s etcdctl get --prefix %s", EtcdContainer, shellQuote(zoneKey(cfg.DNSDomain()))))
	if err != nil {
		return nil, fmt.Errorf("reading records (is the VM up with network.dns enabled?): %w", err)
	}
	return parseRecords(out), nil
}

// parseRecords reads `etcdctl get` output: alternating key and value lines.
func parseRecords(out string) []Record {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var recs []Record
	for i := 0; i+1 < len(lines); i += 2 {
		key, val := strings.TrimSpace(lines[i]), strings.TrimSpace(lines[i+1])
		var v struct {
			Host string `json:"host"`
		}
		if json.Unmarshal([]byte(val), &v) != nil || v.Host == "" {
			continue // TXT ownership record, or not one of ours
		}
		recs = append(recs, Record{Name: keyToName(key), IP: v.Host})
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Name != recs[j].Name {
			return recs[i].Name < recs[j].Name
		}
		return recs[i].IP < recs[j].IP
	})
	return recs
}

// keyToName turns /skydns/internal/marina/dev/default/web/<id> back into
// web.default.dev.marina.internal. The last path segment is ExternalDNS's
// per-target id, not a label.
func keyToName(key string) string {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(key, EtcdPrefix), "/"), "/")
	if len(parts) > 1 {
		parts = parts[:len(parts)-1]
	}
	slices.Reverse(parts)
	return strings.Join(parts, ".")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ServerRunning reports whether the DNS server container is up.
func ServerRunning(ctx context.Context, g *guest.Client) (bool, error) {
	out, err := g.Run(ctx, "docker inspect -f '{{.State.Running}}' "+ServerContainer+" 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// ProbeFromHost sends one real query from the Mac to the DNS server, bypassing
// /etc/resolver, so it tests the network path on its own: the host route, the
// raw-table exemption and the container. Any answer — including NXDOMAIN for
// the probe name — means the path works; only a timeout or refusal fails.
func ProbeFromHost(ctx context.Context, cfg *config.Config) error {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", net.JoinHostPort(cfg.DNSServerIP(), "53"))
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := r.LookupHost(ctx, "marina-probe."+cfg.DNSDomain())
	var dnsErr *net.DNSError
	if err == nil || (errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
		return nil
	}
	return err
}

// ownerRE extracts the owner from an ExternalDNS TXT registry record. The text
// is stored JSON-escaped: "\"heritage=external-dns,external-dns/owner=dev,...\"".
var ownerRE = regexp.MustCompile(`external-dns/owner=([a-z0-9.-]+)`)

// PurgeOwned deletes the records in zone that owner published, and their
// ownership TXT records. Used for a fleet's zone, which several clusters write
// into: deleting the whole prefix would take the other members' names too.
//
// Layout (SkyDNS, label-reversed): the A record for gateway.<zone> is under
// <zone>/gateway/<id>; its TXT registry record is under <zone>/a-gateway/<id>.
func PurgeOwned(ctx context.Context, g *guest.Client, zone, owner string) error {
	out, err := g.Run(ctx, fmt.Sprintf(
		"docker exec %s etcdctl get --prefix %s 2>/dev/null || true", EtcdContainer, shellQuote(zoneKey(zone))))
	if err != nil {
		return err
	}
	for _, k := range ownedKeys(out, owner) {
		if _, err := g.Run(ctx, fmt.Sprintf("docker exec %s etcdctl del %s >/dev/null", EtcdContainer, shellQuote(k))); err != nil {
			return err
		}
	}
	return nil
}

// ownedKeys returns the exact etcd keys to delete for owner: each TXT registry
// record it owns, plus the records stored directly beside it under the same
// name. Exact keys, not prefixes — a prefix would also take deeper names
// (x.gateway.<zone>) that another cluster may own.
func ownedKeys(out, owner string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var all []string
	for i := 0; i+1 < len(lines); i += 2 {
		all = append(all, strings.TrimSpace(lines[i]))
	}
	dirOf := func(k string) string { return k[:strings.LastIndex(k, "/")] }
	var del []string
	for _, txt := range ownedTXTKeys(out, owner) {
		del = append(del, txt)
		dir := dirOf(txt)
		parent, label := dir[:strings.LastIndex(dir, "/")+1], dir[strings.LastIndex(dir, "/")+1:]
		// TXT names are the record name with a record-type prefix: a-, aaaa-, cname-.
		_, rec, ok := strings.Cut(label, "-")
		if !ok {
			continue
		}
		for _, k := range all {
			if dirOf(k) == parent+rec {
				del = append(del, k)
			}
		}
	}
	return del
}

// ownedTXTKeys returns the keys of TXT registry records owned by owner, from
// `etcdctl get` output (alternating key and value lines).
func ownedTXTKeys(out, owner string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var keys []string
	for i := 0; i+1 < len(lines); i += 2 {
		m := ownerRE.FindStringSubmatch(lines[i+1])
		if m != nil && m[1] == owner {
			keys = append(keys, strings.TrimSpace(lines[i]))
		}
	}
	return keys
}

// PurgeZone deletes every record in zone — a fleet's zone once it has no
// members left.
func PurgeZone(ctx context.Context, g *guest.Client, zone string) error {
	_, err := g.Run(ctx, fmt.Sprintf(
		"docker exec %s etcdctl del --prefix %s >/dev/null 2>&1 || true", EtcdContainer, shellQuote(zoneKey(zone))))
	return err
}
