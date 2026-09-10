package config

import (
	"net"
	"sort"
	"strings"
)

// ProxyConfig configures an HTTP(S) proxy for everything klimax runs in the VM:
// dockerd, the registry mirrors, and — via the environment kind inherits — the
// containerd inside every kind node.
//
// Lima already reads the Mac's system proxy settings and writes them into the
// guest's /etc/environment (propagateProxyEnv, on by default), and klimax's own
// SSH commands pick those up through pam_env. That covers anything run from a
// login session. It does NOT cover systemd services: dockerd never reads
// /etc/environment, so without klimax writing a drop-in it pulls directly and
// fails. That gap is what this config exists to close.
type ProxyConfig struct {
	// InheritFromHost uses the Mac's system proxy settings when HTTP/HTTPS are
	// not set explicitly. nil = true.
	InheritFromHost *bool `yaml:"inheritFromHost,omitempty"`
	// HTTP is the proxy URL for plain HTTP, e.g. "http://proxy.corp:3128".
	HTTP string `yaml:"http,omitempty"`
	// HTTPS is the proxy URL for HTTPS. Usually the same value as HTTP —
	// the URL names the proxy, not the scheme it forwards.
	HTTPS string `yaml:"https,omitempty"`
	// NoProxy is appended to the destinations klimax always exempts
	// (see NoProxy). Use it for internal hosts that must bypass the proxy.
	NoProxy []string `yaml:"noProxy,omitempty"`
}

// Enabled reports whether an explicit proxy is configured. It is false when the
// user relies on the Mac's system settings, which Lima propagates on its own.
func (p ProxyConfig) Enabled() bool {
	return p.HTTP != "" || p.HTTPS != ""
}

// InheritsFromHost reports whether unset values should come from macOS.
func (p ProxyConfig) InheritsFromHost() bool {
	return p.InheritFromHost == nil || *p.InheritFromHost
}

// noProxyAlways are the destinations that must never be sent to a proxy,
// whatever the user configures.
//
// A corporate proxy has no route to any of these, so letting cluster-internal
// traffic reach it turns every in-cluster call into a timeout that looks like a
// klimax bug. Computing this list is the main reason proxy support belongs in
// klimax rather than in a documentation page: the user does not know the bridge
// CIDR, the mirror names, or the subnets kind will allocate.
var noProxyAlways = []string{
	"localhost",
	"127.0.0.1",
	"::1",
	// kind's in-cluster DNS.
	".svc",
	".cluster.local",
	// Service and pod subnets. klimax allocates 10.<num>.0.0/16 and
	// 10.1<num>.0.0/16 per cluster, so the whole private range is exempted
	// rather than enumerating a CIDR per possible cluster number.
	//
	// The trade-off is deliberate: on a corporate network that also uses
	// 10.0.0.0/8, those hosts bypass the proxy too. That is usually correct
	// (internal ranges are normally directly routable, and typically already
	// in the site's own no_proxy), and the alternative — omitting it — breaks
	// every cluster klimax creates.
	"10.0.0.0/8",
}

// NoProxy returns the full no_proxy value for the VM: the destinations klimax
// must always exempt, the kind bridge CIDR, the registry mirror names, the
// host↔VM subnet, and finally the user's own additions.
//
// lima0IP is the VM's vzNAT address; pass "" when it is not known yet and the
// host↔VM entry is skipped. The subnet is derived from the address rather than
// hardcoded, because macOS assigns it and the range is not guaranteed.
func (c *Config) NoProxy(lima0IP string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}

	for _, v := range noProxyAlways {
		add(v)
	}

	// The kind bridge: node IPs and MetalLB VIPs live here.
	add(c.Network.KindBridgeCIDR)

	// The host↔VM subnet, as a /24 around the VM's own address.
	if ip := net.ParseIP(lima0IP); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			add((&net.IPNet{IP: v4.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String())
		}
	}

	// Mirror containers are reached by name on the kind network, and by
	// host:port from the guest. Neither is routable from a proxy.
	names := make([]string, 0, len(c.Registries.Mirrors))
	for _, m := range c.Registries.Mirrors {
		names = append(names, m.Name)
	}
	sort.Strings(names) // stable output regardless of config order
	for _, n := range names {
		add(n)
	}

	for _, v := range c.Network.Proxy.NoProxy {
		add(v)
	}
	return out
}

// NoProxyString renders NoProxy as the comma-separated value the environment
// variable takes.
func (c *Config) NoProxyString(lima0IP string) string {
	return strings.Join(c.NoProxy(lima0IP), ",")
}

// ProxyEnv returns the proxy environment variables to set for a guest process,
// or nil when no explicit proxy is configured. Both the lower- and upper-case
// spellings are emitted: Go reads the lower-case names, most other tooling the
// upper-case ones, and a mismatch between them is a classic source of "works in
// curl, fails in the app".
func (c *Config) ProxyEnv(lima0IP string) map[string]string {
	p := c.Network.Proxy
	if !p.Enabled() {
		return nil
	}
	env := map[string]string{}
	set := func(lower, v string) {
		if v == "" {
			return
		}
		env[lower] = v
		env[strings.ToUpper(lower)] = v
	}
	set("http_proxy", p.HTTP)
	set("https_proxy", p.HTTPS)
	set("no_proxy", c.NoProxyString(lima0IP))
	return env
}
