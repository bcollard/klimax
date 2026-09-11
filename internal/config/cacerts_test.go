package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A syntactically valid self-signed certificate is not needed — Load only
// checks PEM framing — but using a real one keeps the test honest.
const testCertPEM = `-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJAKZ3Zq0Zq0ZqMA0GCSqGSIb3DQEBCwUAMBExDzANBgNVBAMMBmts
aW1heDAeFw0yNjAxMDEwMDAwMDBaFw0zNjAxMDEwMDAwMDBaMBExDzANBgNVBAMM
BmtsaW1heDCBnzANBgkqhkiG9w0BAQEFAAOBjQAwgYkCgYEAy8Dbv8prpJ/0kKhl
GeJYozo2t60EG8L0561g13R29LvMR5hyvGZlGJpmn65+A4xHXInJYiPuKzrKUnAp
Gmmm5Nc4Kk5ZqZmZq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Zq0Z
q0Zq0Zq0Zq0Zq0Zq0Zq0ZqUCAwEAATANBgkqhkiG9w0BAQsFAAOBgQAAAAAAAAAA
-----END CERTIFICATE-----
`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCACertsLoad(t *testing.T) {
	p := writeTemp(t, "corp-root.pem", testCertPEM)
	got, err := CACerts{Files: []string{p}}.Load()
	if err != nil {
		t.Fatal(err)
	}
	// update-ca-certificates only reads *.crt, whatever the source was called.
	if _, ok := got["klimax-corp-root.crt"]; !ok {
		t.Errorf("unexpected guest names: %v", SortedNames(got))
	}
}

func TestCACertsDisabledByDefault(t *testing.T) {
	var c CACerts
	if c.Enabled() {
		t.Error("empty CACerts should be disabled")
	}
	got, err := c.Load()
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want nil, nil", got, err)
	}
}

// Pointing at a DER file or a key is a mistake that would otherwise surface much
// later as a pull failure, so it is rejected at load.
func TestCACertsRejectsNonCertificates(t *testing.T) {
	tests := []struct {
		name, content, wantIn string
	}{
		{"der.crt", "\x30\x82\x01\x0a\x02\x82\x01\x01", "no CERTIFICATE block"},
		{"empty.pem", "", "no CERTIFICATE block"},
		{"key.pem", "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n", "not a key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeTemp(t, tt.name, tt.content)
			_, err := CACerts{Files: []string{p}}.Load()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("error %q should mention %q", err, tt.wantIn)
			}
		})
	}
}

func TestCACertsMissingFile(t *testing.T) {
	_, err := CACerts{Files: []string{filepath.Join(t.TempDir(), "nope.pem")}}.Load()
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Errorf("expected a read error, got %v", err)
	}
}

// Two different host files reducing to the same guest name would silently drop
// one of the trust anchors.
func TestCACertsRejectsCollidingGuestNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"corp.pem", "corp.crt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(testCertPEM), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := CACerts{Files: []string{
		filepath.Join(dir, "corp.pem"),
		filepath.Join(dir, "corp.crt"),
	}}.Load()
	if err == nil || !strings.Contains(err.Error(), "rename one") {
		t.Errorf("expected a collision error, got %v", err)
	}
}

func TestGuestCertNameSanitizes(t *testing.T) {
	for in, want := range map[string]string{
		"corp root CA.pem":   "klimax-corp-root-CA.crt",
		"a/b/../evil;rm.pem": "klimax-evil-rm.crt",
		".pem":               "klimax-cert.crt",
	} {
		if got := GuestCertName(in); got != want {
			t.Errorf("GuestCertName(%q) = %q, want %q", in, got, want)
		}
	}
}
