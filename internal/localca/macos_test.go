//go:build darwin

package localca

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMacOSAcceptsTheChain runs Apple's own verifier — the one Safari, Go on
// darwin and `security` use — against every name the wildcard is for, with the
// root as a one-off anchor (no keychain change). Go's crypto/x509 on other
// platforms accepted chains Apple rejects: v0.2.2 constrained intermediates to
// both ".zone" and "zone", and Apple failed the bare "zone" SAN against ".zone".
func TestMacOSAcceptsTheChain(t *testing.T) {
	if _, err := exec.LookPath("security"); err != nil {
		t.Skip("security(1) not available")
	}
	s := newStore(t)
	for _, zone := range []struct {
		name  string
		fleet bool
	}{{"dev", false}, {"lab", true}} {
		var c *Cluster
		var err error
		if zone.fleet {
			c, err = s.EnsureFleet(zone.name)
		} else {
			c, err = s.EnsureCluster(zone.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		chain := parseChain(t, c.WildcardPEM)
		for i, p := range map[int]string{0: "leaf.pem", 1: "inter.pem"} {
			writePEM(t, filepath.Join(dir, p), chain[i].Raw)
		}
		root := filepath.Join(dir, "root.pem")
		if err := os.WriteFile(root, []byte(c.RootPEM), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"web." + zone.name + ".marina.internal", zone.name + ".marina.internal"} {
			out, err := exec.Command("security", "verify-cert", "-r", root,
				"-c", filepath.Join(dir, "leaf.pem"), "-c", filepath.Join(dir, "inter.pem"),
				"-p", "ssl", "-s", host).CombinedOutput()
			if err != nil {
				t.Errorf("macOS rejects the %s wildcard for %s: %v\n%s", zone.name, host, err, out)
			}
		}
	}
}
