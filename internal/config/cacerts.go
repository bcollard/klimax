package config

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lima-vm/lima/v2/pkg/localpathutil"
)

// CACerts lists extra certificate authorities the VM and its clusters should
// trust: a proxy that terminates TLS (corporate interception), or a private
// registry with a self-signed chain.
//
// Without these, a TLS-intercepting proxy fails every pull with
// "x509: certificate signed by unknown authority" even when network.proxy is
// correct — the proxy is reachable, its certificate simply is not trusted.
type CACerts struct {
	// Files are host paths to PEM files. A leading "~" is expanded.
	Files []string `yaml:"files,omitempty"`
}

// Enabled reports whether any CA certificate is configured.
func (c CACerts) Enabled() bool { return len(c.Files) > 0 }

// GuestCertName is the filename a host PEM takes in the guest's
// /usr/local/share/ca-certificates. update-ca-certificates only picks up files
// ending in .crt, whatever the source was called.
func GuestCertName(hostPath string) string {
	base := filepath.Base(hostPath)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	// Namespaced so klimax's certificates are identifiable, and removable,
	// without touching anything the image shipped with.
	return "klimax-" + sanitizeCertName(base) + ".crt"
}

// sanitizeCertName reduces a filename to characters that are safe in a shell
// path, since the name is interpolated into guest commands.
func sanitizeCertName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "cert"
	}
	return out
}

// Load reads every configured PEM file, returning guest filename -> contents.
//
// The contents are validated as PEM here rather than left to fail inside the
// guest: pointing at a DER file, a private key, or a path that does not exist
// otherwise surfaces much later as a pull failure that looks like a network
// problem.
func (c CACerts) Load() (map[string]string, error) {
	if !c.Enabled() {
		return nil, nil
	}
	out := make(map[string]string, len(c.Files))
	for _, f := range c.Files {
		path, err := localpathutil.Expand(f)
		if err != nil {
			return nil, fmt.Errorf("vm.caCerts: expanding %q: %w", f, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("vm.caCerts: reading %q: %w", path, err)
		}
		if err := validateCertPEM(data); err != nil {
			return nil, fmt.Errorf("vm.caCerts: %q: %w", path, err)
		}
		name := GuestCertName(path)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("vm.caCerts: two entries map to the guest file %q; rename one", name)
		}
		out[name] = string(data)
	}
	return out, nil
}

// SortedNames returns the guest filenames in a stable order, so anything derived
// from them (drift comparison, generated scripts) does not change run to run.
func SortedNames(certs map[string]string) []string {
	names := make([]string, 0, len(certs))
	for n := range certs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// validateCertPEM checks that data contains at least one CERTIFICATE block and
// nothing that obviously should not be handed out as a trust anchor.
func validateCertPEM(data []byte) error {
	rest := data
	found := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE":
			found = true
		case strings.Contains(block.Type, "PRIVATE KEY"):
			// Distributing a private key to every cluster node is worth
			// refusing outright rather than warning about.
			return fmt.Errorf("contains a %s block — this should be a certificate, not a key", block.Type)
		}
	}
	if !found {
		return fmt.Errorf("no CERTIFICATE block found (a DER file needs converting: openssl x509 -inform der -in cert.der -out cert.pem)")
	}
	return nil
}
