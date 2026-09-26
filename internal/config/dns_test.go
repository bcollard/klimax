package config

import (
	"strings"
	"testing"
)

func TestDNSDefaults(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if !cfg.DNSEnabled() {
		t.Fatal("network.dns should default to enabled")
	}
	if got := cfg.DNSDomain(); got != "klimax.internal" {
		t.Errorf("domain = %q, want klimax.internal", got)
	}
	if got := cfg.DNSServerIP(); got != "172.30.255.53" {
		t.Errorf("server = %q, want 172.30.255.53", got)
	}
	if got := cfg.DNSEtcdIP(); got != "172.30.255.52" {
		t.Errorf("etcd = %q, want 172.30.255.52", got)
	}
	if got := cfg.ClusterDNSZone("dev"); got != "dev.klimax.internal" {
		t.Errorf("zone = %q", got)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("default config should validate: %v", err)
	}
}

func TestDNSExplicitlyDisabled(t *testing.T) {
	cfg := &Config{Network: NetworkConfig{DNS: DNSConfig{Enabled: boolPtr(false), Domain: "bad"}}}
	applyDefaults(cfg)
	if cfg.DNSEnabled() {
		t.Fatal("explicit false must win over the default")
	}
	// A bad domain on a disabled feature is not worth failing `up` over.
	if err := Validate(cfg); err != nil {
		t.Errorf("disabled DNS should skip DNS validation: %v", err)
	}
}

func TestDNSDomainNormalized(t *testing.T) {
	cfg := &Config{Network: NetworkConfig{DNS: DNSConfig{Domain: " Lab.Internal. "}}}
	if got := cfg.DNSDomain(); got != "lab.internal" {
		t.Errorf("DNSDomain() = %q, want lab.internal", got)
	}
}

func TestDNSValidation(t *testing.T) {
	cases := []struct {
		name, domain, cidr, wantErr string
	}{
		{"bare TLD", "internal", "", "at least two labels"},
		{"mdns", "klimax.local", "", ".local is reserved"},
		{"bad label", "kli_max.internal", "", "not a valid DNS label"},
		{"cidr too small", "", "172.30.0.0/24", "inside network.kindBridgeCIDR"},
		{"custom ok", "lab.internal", "", ""},
		{"other /16 ok", "", "10.200.0.0/16", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Network: NetworkConfig{KindBridgeCIDR: tc.cidr, DNS: DNSConfig{Domain: tc.domain}}}
			applyDefaults(cfg)
			err := Validate(cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestNoProxyIncludesDNSZone(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if !strings.Contains(cfg.NoProxyString(""), ".klimax.internal") {
		t.Errorf("no_proxy should exempt the local zone: %s", cfg.NoProxyString(""))
	}
	cfg.Network.DNS.Enabled = boolPtr(false)
	if strings.Contains(cfg.NoProxyString(""), "klimax.internal") {
		t.Errorf("no_proxy should not list the zone when DNS is off")
	}
}

func TestDNSNameTemplate(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if got := cfg.DNSNameExample("dev"); got != "<service>.<namespace>.dev.klimax.internal" {
		t.Errorf("default example = %q", got)
	}
	cfg.Network.DNS.NameTemplate = "{{.Name}}-{{.Namespace}}"
	if got := cfg.DNSNameExample("dev"); got != "<service>-<namespace>.dev.klimax.internal" {
		t.Errorf("flat example = %q", got)
	}
	for tmpl, wantErr := range map[string]string{
		"{{.Name}}-{{.Namespace}}": "",
		"{{.Name}}":                "",
		"{{.Name":                  "nameTemplate",
		"{{.Name}}_{{.Namespace}}": "not a valid relative DNS name",
		"{{.Name}}.":               "not a valid relative DNS name",
	} {
		cfg.Network.DNS.NameTemplate = tmpl
		err := Validate(cfg)
		switch {
		case wantErr == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tmpl, err)
		case wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)):
			t.Errorf("%q: error = %v, want %q", tmpl, err, wantErr)
		}
	}
}

func TestTLSEnabled(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if !cfg.TLSEnabled() {
		t.Error("tls should default to enabled")
	}
	cfg.Network.DNS.Enabled = boolPtr(false)
	if cfg.TLSEnabled() {
		t.Error("tls must be off when the DNS zone is off")
	}
}
