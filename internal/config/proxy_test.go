package config

import (
	"slices"
	"strings"
	"testing"
)

func proxyTestConfig() *Config {
	return &Config{
		Network: NetworkConfig{KindBridgeCIDR: "172.30.0.0/16"},
		Registries: RegistryConfig{Mirrors: []RegistryMirror{
			{Name: "registry-quayio", Port: 5010},
			{Name: "registry-dockerio", Port: 5030},
		}},
	}
}

func TestNoProxyCoversClusterInternalTraffic(t *testing.T) {
	got := proxyTestConfig().NoProxy("192.168.64.3")

	// Each of these, sent to a corporate proxy, becomes a timeout that looks
	// like a klimax bug rather than a proxy misconfiguration.
	for _, want := range []string{
		"localhost", "127.0.0.1", "::1",
		".svc", ".cluster.local",
		"10.0.0.0/8",       // service + pod subnets
		"172.30.0.0/16",    // kind bridge: node IPs and MetalLB VIPs
		"192.168.64.0/24",  // host <-> VM
		"registry-dockerio", "registry-quayio",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("no_proxy is missing %q:\n  %s", want, strings.Join(got, ","))
		}
	}
}

// The vzNAT subnet is assigned by macOS and is not guaranteed to be
// 192.168.64.x, so it must be derived from the address rather than hardcoded.
func TestNoProxyDerivesHostSubnetFromLima0IP(t *testing.T) {
	got := proxyTestConfig().NoProxy("192.168.105.7")
	if !slices.Contains(got, "192.168.105.0/24") {
		t.Errorf("expected the /24 around the VM address:\n  %s", strings.Join(got, ","))
	}
	if slices.Contains(got, "192.168.64.0/24") {
		t.Error("hardcoded 192.168.64.0/24 leaked in")
	}
}

func TestNoProxySkipsHostSubnetWhenIPUnknown(t *testing.T) {
	for _, ip := range []string{"", "not-an-ip"} {
		for _, e := range proxyTestConfig().NoProxy(ip) {
			if strings.HasPrefix(e, "192.168.") {
				t.Errorf("NoProxy(%q) invented a host subnet: %q", ip, e)
			}
		}
	}
}

func TestNoProxyAppendsUserEntriesAndDeduplicates(t *testing.T) {
	cfg := proxyTestConfig()
	cfg.Network.Proxy.NoProxy = []string{"*.corp.example", "localhost", " 10.0.0.0/8 "}

	got := cfg.NoProxy("192.168.64.3")
	if !slices.Contains(got, "*.corp.example") {
		t.Error("user entry dropped")
	}
	count := map[string]int{}
	for _, e := range got {
		count[e]++
	}
	for _, dup := range []string{"localhost", "10.0.0.0/8"} {
		if count[dup] != 1 {
			t.Errorf("%q appears %d times: %s", dup, count[dup], strings.Join(got, ","))
		}
	}
}

// Output feeds a systemd drop-in and container env; reordering it on every run
// would make drift detection see changes that are not there.
func TestNoProxyIsStableAcrossMirrorOrder(t *testing.T) {
	a := proxyTestConfig()
	b := proxyTestConfig()
	b.Registries.Mirrors = []RegistryMirror{
		{Name: "registry-dockerio", Port: 5030},
		{Name: "registry-quayio", Port: 5010},
	}
	if x, y := a.NoProxyString("192.168.64.3"), b.NoProxyString("192.168.64.3"); x != y {
		t.Errorf("order-dependent output:\n  %s\n  %s", x, y)
	}
}

func TestProxyEnvNilWhenNotConfigured(t *testing.T) {
	cfg := proxyTestConfig()
	if env := cfg.ProxyEnv("192.168.64.3"); env != nil {
		t.Errorf("no proxy configured should yield nil, got %v", env)
	}
	if cfg.Network.Proxy.Enabled() {
		t.Error("Enabled() should be false with no proxy set")
	}
}

// Go reads the lower-case names, most other tooling the upper-case ones. Setting
// only one spelling is a classic "works in curl, fails in the app".
func TestProxyEnvSetsBothCases(t *testing.T) {
	cfg := proxyTestConfig()
	cfg.Network.Proxy.HTTP = "http://proxy.corp:3128"
	cfg.Network.Proxy.HTTPS = "http://proxy.corp:3128"

	env := cfg.ProxyEnv("192.168.64.3")
	for _, k := range []string{
		"http_proxy", "HTTP_PROXY",
		"https_proxy", "HTTPS_PROXY",
		"no_proxy", "NO_PROXY",
	} {
		if env[k] == "" {
			t.Errorf("missing %s in %v", k, env)
		}
	}
	if env["http_proxy"] != env["HTTP_PROXY"] {
		t.Error("the two spellings disagree")
	}
	if !strings.Contains(env["no_proxy"], "172.30.0.0/16") {
		t.Errorf("no_proxy lost the bridge CIDR: %q", env["no_proxy"])
	}
}

// Only https set is legitimate — the URL names the proxy, not the scheme.
func TestProxyEnvHTTPSOnly(t *testing.T) {
	cfg := proxyTestConfig()
	cfg.Network.Proxy.HTTPS = "http://proxy.corp:3128"

	env := cfg.ProxyEnv("192.168.64.3")
	if env["https_proxy"] == "" {
		t.Fatal("https_proxy not set")
	}
	if _, ok := env["http_proxy"]; ok {
		t.Error("http_proxy invented from an unset value")
	}
}

func TestInheritsFromHostDefaultsToTrue(t *testing.T) {
	var p ProxyConfig
	if !p.InheritsFromHost() {
		t.Error("unset inheritFromHost should mean true")
	}
	f := false
	p.InheritFromHost = &f
	if p.InheritsFromHost() {
		t.Error("explicit false should be honoured")
	}
}
