package localdns

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseManagedDNS(t *testing.T) {
	cases := []struct {
		name, json, protocol, server string
	}{
		{"DoH nested under DNSSettings",
			`{"PayloadType":"com.apple.dnsSettings.managed","DNSSettings":{"DNSProtocol":"HTTPS","ServerURL":"https://dns.quad9.net/dns-query"}}`,
			"HTTPS", "https://dns.quad9.net/dns-query"},
		{"DoH at the top level",
			`{"DNSProtocol":"HTTPS","ServerURL":"https://dns.quad9.net/dns-query"}`,
			"HTTPS", "https://dns.quad9.net/dns-query"},
		{"DoT with addresses",
			`{"DNSSettings":{"DNSProtocol":"TLS","ServerName":"dns.example","ServerAddresses":["9.9.9.9"]}}`,
			"TLS", "dns.example"},
		{"addresses only",
			`{"DNSSettings":{"DNSProtocol":"TLS","ServerAddresses":["9.9.9.9"]}}`,
			"TLS", "9.9.9.9"},
		{"not JSON", `nope`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, s := parseManagedDNS([]byte(c.json))
			if p != c.protocol || s != c.server {
				t.Errorf("got (%q, %q), want (%q, %q)", p, s, c.protocol, c.server)
			}
		})
	}
}

func TestProbeKeyRoundTrips(t *testing.T) {
	cfg := testConfig(t)
	if got, want := keyToName(probeKey(cfg)), ProbeName(cfg); got != want {
		t.Errorf("keyToName(probeKey) = %q, want %q", got, want)
	}
}

func TestFindManagedDNSProfiles(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil is macOS-only")
	}
	dir := t.TempDir()
	old := managedPrefsDir
	managedPrefsDir = dir
	t.Cleanup(func() { managedPrefsDir = old })

	if got := FindManagedDNSProfiles(); len(got) != 0 {
		t.Fatalf("empty dir: got %v", got)
	}

	// The shape Kandji materialised on a colleague's Mac.
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>DNSSettings</key><dict>
    <key>DNSProtocol</key><string>HTTPS</string>
    <key>ServerURL</key><string>https://dns.quad9.net/dns-query</string>
  </dict>
</dict></plist>`
	path := filepath.Join(dir, managedDNSPlist)
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	got := FindManagedDNSProfiles()
	want := ManagedDNSProfile{Path: path, Protocol: "HTTPS", Server: "https://dns.quad9.net/dns-query"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %+v, want [%+v]", got, want)
	}
}
