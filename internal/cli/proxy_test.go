package cli

import (
	"strings"
	"testing"

	"github.com/bcollard/klimax/internal/config"
	"github.com/bcollard/klimax/internal/limatemplate"
)

func proxyCfg() *config.Config {
	return &config.Config{
		Network: config.NetworkConfig{KindBridgeCIDR: "172.30.0.0/16"},
		Registries: config.RegistryConfig{Mirrors: []config.RegistryMirror{
			{Name: "registry-dockerio", Port: 5030},
		}},
	}
}

func TestDockerProxyDropInEmptyWithoutProxy(t *testing.T) {
	if got := limatemplate.DockerProxyDropIn(proxyCfg(), "192.168.64.3"); got != "" {
		t.Errorf("expected no drop-in when no proxy is set, got:\n%s", got)
	}
}

func TestDockerProxyDropInIsValidUnitSyntaxAndDeterministic(t *testing.T) {
	cfg := proxyCfg()
	cfg.Network.Proxy.HTTP = "http://proxy.corp:3128"
	cfg.Network.Proxy.HTTPS = "http://proxy.corp:3128"

	got := limatemplate.DockerProxyDropIn(cfg, "192.168.64.3")

	if !strings.Contains(got, "[Service]") {
		t.Errorf("missing [Service] section:\n%s", got)
	}
	// systemd needs Environment="K=V"; an unquoted no_proxy with commas would
	// be parsed as multiple assignments.
	for _, want := range []string{
		`Environment="HTTP_PROXY=http://proxy.corp:3128"`,
		`Environment="http_proxy=http://proxy.corp:3128"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `Environment="no_proxy=`) {
		t.Errorf("no_proxy not quoted as one value:\n%s", got)
	}
	// Rewritten on every `up`; unstable ordering would restart Docker each time.
	if second := limatemplate.DockerProxyDropIn(cfg, "192.168.64.3"); second != got {
		t.Error("output is not deterministic across calls")
	}
}

func TestParseEnvironmentProxy(t *testing.T) {
	// Shaped like the real file: a PATH line, Lima's markers, and quoting.
	const content = `PATH="/usr/local/sbin:/usr/local/bin:/usr/bin"
#LIMA-START
http_proxy=http://192.168.5.2:3128
HTTPS_PROXY="http://192.168.5.2:3128"
no_proxy=localhost
#LIMA-END
`
	got := parseEnvironmentProxy(content)
	if got["http_proxy"] != "http://192.168.5.2:3128" {
		t.Errorf("http_proxy: got %q", got["http_proxy"])
	}
	// Upper-case spelling must normalise to the lower-case key.
	if got["https_proxy"] != "http://192.168.5.2:3128" {
		t.Errorf("https_proxy: got %q", got["https_proxy"])
	}
	// no_proxy is klimax's to compute, never inherited.
	if _, ok := got["no_proxy"]; ok {
		t.Error("no_proxy should not be lifted from the guest")
	}
	if _, ok := got["path"]; ok {
		t.Error("PATH leaked in")
	}
}

func TestParseEnvironmentProxyEmptyWhenNoProxySet(t *testing.T) {
	const content = "PATH=\"/usr/bin\"\n#LIMA-START\n#LIMA-END\n"
	if got := parseEnvironmentProxy(content); len(got) != 0 {
		t.Errorf("expected nothing, got %v", got)
	}
	// An empty value means "explicitly unset" and must not become a proxy.
	if got := parseEnvironmentProxy("http_proxy=\n"); len(got) != 0 {
		t.Errorf("empty value should be ignored, got %v", got)
	}
}
