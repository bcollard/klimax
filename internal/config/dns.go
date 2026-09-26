package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"text/template"
)

// DNSConfig controls klimax's local DNS zone for LoadBalancer Services.
//
// With it on, every cluster publishes its LoadBalancer Services and Ingress
// hosts as <svc>.<namespace>.<cluster>.<domain>, resolvable from the Mac, the
// VM and every pod — without owning a domain. The pieces:
//
//   - an etcd and a CoreDNS container on the kind network, at fixed addresses
//     (EtcdIP / ServerIP) derived from network.kindBridgeCIDR;
//   - ExternalDNS in each cluster, writing records into that etcd;
//   - /etc/resolver/<domain> on the Mac, pointing at ServerIP. The address is
//     on the kind network, which the host route already follows across lima0
//     IP changes, so the file is written once and never goes stale.
//
// Reconciled on every `klimax up`: turning it off removes the containers, the
// iptables exemption and the resolver file. ExternalDNS is installed at
// cluster creation, so it reaches clusters created while the toggle is on.
type DNSConfig struct {
	// Enabled is the global toggle. nil = default (true).
	Enabled *bool `yaml:"enabled"`
	// Domain is the zone klimax serves. Default "klimax.internal".
	//
	// .internal is reserved by ICANN for private use and never delegated in
	// the public root. klimax takes a subdomain rather than all of .internal,
	// because corporate networks and GCP (metadata.google.internal) already use
	// it and a resolver file for the whole TLD would take those names over.
	Domain string `yaml:"domain"`
	// NameTemplate is the automatic name every LoadBalancer Service and Ingress
	// gets, as an ExternalDNS --fqdn-template relative to the cluster's zone:
	// klimax appends ".<cluster>.<domain>". Fields: .Name, .Namespace, .Labels,
	// .Annotations. Default "{{.Name}}.{{.Namespace}}", mirroring Kubernetes'
	// own <svc>.<ns>.svc.cluster.local.
	//
	// A wildcard certificate covers one label, so the cluster's
	// *.<cluster>.<domain> wildcard does not cover these automatic names — it
	// covers names directly under the cluster zone: annotated names and
	// Ingress hosts such as shop.<cluster>.<domain>. "{{.Name}}-{{.Namespace}}"
	// flattens the automatic names into that one label, at the cost of
	// ambiguity (a-b in c vs a in b-c) and a shared 63-character label.
	NameTemplate string `yaml:"nameTemplate"`
	// TLS runs a local CA for the zone. See TLSConfig.
	TLS TLSConfig `yaml:"tls"`
}

// TLSConfig controls klimax's local certificate authority for the DNS zone.
//
// A root CA on the Mac, name-constrained to .<domain>, trusted in the System
// keychain; one intermediate per cluster, constrained to .<cluster>.<domain>;
// and a *.<cluster>.<domain> wildcard certificate signed by it and stored in
// the cluster as the TLS Secret default/klimax-wildcard-tls. The constraints
// are what make trusting the root safe: nothing it or its intermediates sign
// is accepted for any other domain, and a leaked intermediate only covers its
// own cluster.
type TLSConfig struct {
	// Enabled is the toggle. nil = default (true). Has no effect when
	// network.dns is disabled.
	Enabled *bool `yaml:"enabled"`
}

// DefaultDNSDomain is the zone served when network.dns.domain is unset.
const DefaultDNSDomain = "klimax.internal"

// DefaultDNSNameTemplate is the automatic per-Service name, relative to the
// cluster zone.
const DefaultDNSNameTemplate = "{{.Name}}.{{.Namespace}}"

// dnsHostOctets are the last two octets of the DNS containers' addresses.
// x.y.255.0/24 is outside every MetalLB pool (x.y.<num>.* for num 1–99) and far
// above where Docker's IPAM starts handing out container addresses (x.y.0.2).
const (
	dnsEtcdSuffix   = "255.52"
	dnsServerSuffix = "255.53"
)

// DNSEnabled reports whether the local DNS zone is on. It defaults to true (the
// field is nil-defaulted in applyDefaults; a nil pointer here — a Config built
// without applyDefaults — is also treated as the default).
func (c *Config) DNSEnabled() bool {
	return c.Network.DNS.Enabled == nil || *c.Network.DNS.Enabled
}

// DNSDomain returns the configured zone, or the default.
func (c *Config) DNSDomain() string {
	if d := strings.TrimSuffix(strings.TrimSpace(c.Network.DNS.Domain), "."); d != "" {
		return strings.ToLower(d)
	}
	return DefaultDNSDomain
}

// DNSNameTemplate returns the configured automatic-name template, or the default.
func (c *Config) DNSNameTemplate() string {
	if t := strings.TrimSpace(c.Network.DNS.NameTemplate); t != "" {
		return t
	}
	return DefaultDNSNameTemplate
}

// TLSEnabled reports whether the local CA is on. It needs the DNS zone: the CA
// is constrained to it and issues for its names.
func (c *Config) TLSEnabled() bool {
	return c.DNSEnabled() && (c.Network.DNS.TLS.Enabled == nil || *c.Network.DNS.TLS.Enabled)
}

// DNSServerIP is the CoreDNS container's address on the kind network — the one
// the Mac, the VM and the clusters' CoreDNS forward the zone to.
func (c *Config) DNSServerIP() string { return c.dnsIP(dnsServerSuffix) }

// DNSEtcdIP is the etcd container's address. Only the clusters' ExternalDNS and
// CoreDNS talk to it; it is deliberately not exempted for the Mac.
func (c *Config) DNSEtcdIP() string { return c.dnsIP(dnsEtcdSuffix) }

func (c *Config) dnsIP(suffix string) string {
	ip, _, err := net.ParseCIDR(c.Network.KindBridgeCIDR)
	if err != nil {
		return ""
	}
	v4 := ip.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%s", v4[0], v4[1], suffix)
}

// ClusterDNSZone is the subzone one cluster's ExternalDNS may write to. Each
// cluster owns a disjoint subzone, which is what makes `policy: sync` safe:
// one cluster can never delete another's records.
func (c *Config) ClusterDNSZone(cluster string) string {
	return cluster + "." + c.DNSDomain()
}

// FleetDNSZone is a fleet's shared zone, for fleet-wide names such as
// kong-gw.<fleet>.<domain>. It sits beside the cluster zones, which is why a
// fleet may not share its name with a cluster.
func (c *Config) FleetDNSZone(fleet string) string {
	return fleet + "." + c.DNSDomain()
}

var dnsLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validateDNS checks network.dns. Only called when the feature is on: a bad
// domain on a disabled feature is not worth failing `klimax up` over.
func validateDNS(c *Config) []error {
	var errs []error
	domain := c.DNSDomain()
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		errs = append(errs, fmt.Errorf("network.dns.domain %q must have at least two labels (e.g. %q) — a resolver file for a bare TLD would capture every name under it", domain, DefaultDNSDomain))
	}
	for _, l := range labels {
		if !dnsLabelRE.MatchString(l) {
			errs = append(errs, fmt.Errorf("network.dns.domain %q: %q is not a valid DNS label", domain, l))
			break
		}
	}
	// .local is multicast DNS (RFC 6762). macOS sends it to mDNSResponder first,
	// and Go and Linux clients treat it as mDNS-only, so names would resolve in
	// some tools and not others.
	if labels[len(labels)-1] == "local" {
		errs = append(errs, fmt.Errorf("network.dns.domain %q: .local is reserved for multicast DNS and resolves inconsistently — use a subdomain of .internal", domain))
	}

	errs = append(errs, validateNameTemplate(c.DNSNameTemplate())...)

	_, cidr, err := net.ParseCIDR(c.Network.KindBridgeCIDR)
	if err == nil {
		for _, ip := range []string{c.DNSEtcdIP(), c.DNSServerIP()} {
			if p := net.ParseIP(ip); p == nil || !cidr.Contains(p) {
				errs = append(errs, fmt.Errorf("network.dns needs %s inside network.kindBridgeCIDR %s — use a /16 kind network, or set network.dns.enabled: false", ip, c.Network.KindBridgeCIDR))
			}
		}
	}
	return errs
}

// validateNameTemplate parses the template and renders it for a sample Service,
// so a typo fails `klimax up` instead of silently publishing nothing.
func validateNameTemplate(tmpl string) []error {
	t, err := template.New("nameTemplate").Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return []error{fmt.Errorf("network.dns.nameTemplate %q: %w", tmpl, err)}
	}
	var b strings.Builder
	sample := map[string]any{"Name": "web", "Namespace": "default", "Labels": map[string]string{}, "Annotations": map[string]string{}}
	if err := t.Execute(&b, sample); err != nil {
		return []error{fmt.Errorf("network.dns.nameTemplate %q: %w", tmpl, err)}
	}
	out := b.String()
	if out == "" {
		return []error{fmt.Errorf("network.dns.nameTemplate %q renders to an empty name", tmpl)}
	}
	for _, l := range strings.Split(out, ".") {
		if !dnsLabelRE.MatchString(l) {
			return []error{fmt.Errorf("network.dns.nameTemplate %q renders %q for a sample Service — not a valid relative DNS name (klimax appends the cluster zone itself)", tmpl, out)}
		}
	}
	return nil
}

// DNSNameExample renders the automatic name with placeholders, for messages:
// "<service>-<namespace>.dev.klimax.internal". cluster "" gives "<cluster>".
func (c *Config) DNSNameExample(cluster string) string {
	if cluster == "" {
		cluster = "<cluster>"
	}
	var b strings.Builder
	t, err := template.New("x").Option("missingkey=zero").Parse(c.DNSNameTemplate())
	if err != nil || t.Execute(&b, map[string]any{"Name": "<service>", "Namespace": "<namespace>"}) != nil {
		return "<service>." + cluster + "." + c.DNSDomain()
	}
	return b.String() + "." + cluster + "." + c.DNSDomain()
}
