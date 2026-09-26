package localca

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir(), "klimax.internal")
}

func parseChain(t *testing.T, pemData string) []*x509.Certificate {
	t.Helper()
	var out []*x509.Certificate
	for rest := []byte(pemData); ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			return out
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
}

// verify checks name against the wildcard chain, trusting only the root —
// exactly what a browser with the root in its keychain does.
func verify(t *testing.T, c *Cluster, name string) error {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(parseChain(t, c.RootPEM)[0])
	chain := parseChain(t, c.WildcardPEM)
	inters := x509.NewCertPool()
	for _, ic := range chain[1:] {
		inters.AddCert(ic)
	}
	_, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inters, DNSName: name,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestRootIsConstrainedAndReused(t *testing.T) {
	s := newStore(t)
	root, _, created, err := s.EnsureRoot()
	if err != nil || !created {
		t.Fatalf("first EnsureRoot: created=%v err=%v", created, err)
	}
	if !root.IsCA || root.MaxPathLen != 1 {
		t.Errorf("root: IsCA=%v MaxPathLen=%d, want CA with pathlen 1", root.IsCA, root.MaxPathLen)
	}
	if !root.PermittedDNSDomainsCritical || !constrainedTo(root, ".klimax.internal") {
		t.Errorf("root constraints = %v (critical=%v)", root.PermittedDNSDomains, root.PermittedDNSDomainsCritical)
	}
	again, _, created, err := s.EnsureRoot()
	if err != nil || created || !again.Equal(root) {
		t.Fatalf("second EnsureRoot must reuse the root: created=%v err=%v", created, err)
	}
	info, err := os.Stat(s.rootKeyPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("root key mode = %v, want 0600", info.Mode().Perm())
	}
	if d, _ := os.Stat(filepath.Dir(s.rootKeyPath())); d.Mode().Perm() != 0o700 {
		t.Errorf("private dir mode = %v, want 0700", d.Mode().Perm())
	}
}

func TestRootForAnotherDomainIsRefused(t *testing.T) {
	home := t.TempDir()
	a := New(home, "klimax.internal")
	if _, _, _, err := a.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	// Point another domain's store at the same files.
	b := &Store{Dir: a.Dir, Domain: "lab.internal"}
	if _, _, _, err := b.EnsureRoot(); err == nil || !strings.Contains(err.Error(), "not constrained to .lab.internal") {
		t.Fatalf("err = %v, want a constraint mismatch", err)
	}
}

func TestClusterWildcardCoversTheZone(t *testing.T) {
	s := newStore(t)
	c, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	for name, ok := range map[string]bool{
		"web-default.dev.klimax.internal":     true, // the flat nameTemplate
		"shop.dev.klimax.internal":            true, // annotated names and Ingress hosts
		"dev.klimax.internal":                 true,
		"web.default.dev.klimax.internal":     false, // the default automatic name: a wildcard covers one label only
		"web-default.staging.klimax.internal": false,
		"github.com":                          false,
	} {
		if err := verify(t, c, name); (err == nil) != ok {
			t.Errorf("%s: verify err=%v, want ok=%v", name, err, ok)
		}
	}
	if len(parseChain(t, c.WildcardPEM)) != 2 {
		t.Error("wildcard.crt must carry the intermediate after the leaf, or root-only clients cannot build the chain")
	}
	if _, err := tls.X509KeyPair([]byte(c.WildcardPEM), []byte(c.WildcardKeyPEM)); err != nil {
		t.Errorf("wildcard cert/key do not form a TLS key pair: %v", err)
	}
}

// TestIntermediateCannotSignOutsideItsZone is the property that makes putting
// the intermediate's key into the cluster acceptable: anything it signs for
// another name is rejected by the client, even though signing itself succeeds.
func TestIntermediateCannotSignOutsideItsZone(t *testing.T) {
	s := newStore(t)
	c, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	inter := parseChain(t, c.ChainPEM)[0]
	if !constrainedTo(inter, ".dev.klimax.internal") || inter.MaxPathLen != 0 || !inter.MaxPathLenZero {
		t.Fatalf("intermediate: constraints=%v pathlen=%d", inter.PermittedDNSDomains, inter.MaxPathLen)
	}
	for _, name := range []string{"github.com", "web.staging.klimax.internal"} {
		evil := signWith(t, s, "dev", name)
		roots := x509.NewCertPool()
		roots.AddCert(parseChain(t, c.RootPEM)[0])
		inters := x509.NewCertPool()
		inters.AddCert(inter)
		_, err := evil.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inters, DNSName: name})
		var invalid x509.CertificateInvalidError
		if err == nil || !asInvalid(err, &invalid) || invalid.Reason != x509.CANotAuthorizedForThisName {
			t.Errorf("%s signed by the dev intermediate: err=%v, want CANotAuthorizedForThisName", name, err)
		}
	}
}

func TestEnsureClusterIsIdempotentAndRenews(t *testing.T) {
	s := newStore(t)
	first, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	if first.WildcardPEM != second.WildcardPEM || first.ChainPEM != second.ChainPEM {
		t.Error("a second EnsureCluster must not re-issue anything")
	}

	// A wildcard inside the renewal window is re-issued, under the same intermediate.
	expiring := signWildcardExpiring(t, s, "dev", 10*24*time.Hour)
	if err := os.WriteFile(filepath.Join(s.clusterDir("dev"), "wildcard.crt"), expiring, 0o644); err != nil {
		t.Fatal(err)
	}
	renewed, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	if renewed.WildcardPEM == first.WildcardPEM || renewed.ChainPEM != first.ChainPEM {
		t.Error("an expiring wildcard should be re-issued and the intermediate kept")
	}
	if time.Until(renewed.WildcardNotAfter) < 300*24*time.Hour {
		t.Errorf("renewed wildcard expires %v", renewed.WildcardNotAfter)
	}
}

func TestNewRootOrphansAndReplacesClusterMaterial(t *testing.T) {
	s := newStore(t)
	old, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.Dir, "private")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.RootCertPath()); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.EnsureCluster("dev")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ChainPEM == old.ChainPEM || fresh.WildcardPEM == old.WildcardPEM {
		t.Fatal("material signed by a replaced root must be re-issued")
	}
	if err := verify(t, fresh, "shop.dev.klimax.internal"); err != nil {
		t.Errorf("re-issued wildcard does not verify: %v", err)
	}
}

func TestRemoveCluster(t *testing.T) {
	s := newStore(t)
	for _, c := range []string{"dev", "staging"} {
		if _, err := s.EnsureCluster(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Clusters(); len(got) != 2 {
		t.Fatalf("Clusters() = %v", got)
	}
	if err := s.RemoveCluster("dev"); err != nil {
		t.Fatal(err)
	}
	if got := s.Clusters(); len(got) != 1 || got[0] != "staging" {
		t.Errorf("after remove: %v", got)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b"} {
		if err := s.RemoveCluster(bad); err == nil {
			t.Errorf("RemoveCluster(%q) should be refused", bad)
		}
	}
	if _, err := os.Stat(s.RootCertPath()); err != nil {
		t.Error("removing a cluster must keep the root")
	}
}

// TestFleetZoneIsIsolated: the fleet intermediate covers the fleet's names and
// nothing else, and a member's own intermediate cannot sign fleet names —
// fleet-wide trust never widens what one cluster can mint.
func TestFleetZoneIsIsolated(t *testing.T) {
	s := newStore(t)
	f, err := s.EnsureFleet("lab")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FleetZone || f.SecretName() != FleetWildcardSecret || f.IssuerName() != FleetIssuerName {
		t.Errorf("fleet material: kind=%s secret=%s issuer=%s", f.Kind, f.SecretName(), f.IssuerName())
	}
	for name, ok := range map[string]bool{
		"gateway.lab.klimax.internal":      true,
		"gateway.lab-east.klimax.internal": false,
		"github.com":                       false,
	} {
		if err := verify(t, f, name); (err == nil) != ok {
			t.Errorf("fleet wildcard for %s: err=%v, want ok=%v", name, err, ok)
		}
	}

	if _, err := s.EnsureCluster("lab-east"); err != nil {
		t.Fatal(err)
	}
	evil := signWith(t, s, "lab-east", "gateway.lab.klimax.internal")
	roots := x509.NewCertPool()
	roots.AddCert(parseChain(t, f.RootPEM)[0])
	inter, _ := clusterCA(t, s, "lab-east")
	inters := x509.NewCertPool()
	inters.AddCert(inter)
	if _, err := evil.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inters, DNSName: "gateway.lab.klimax.internal"}); err == nil {
		t.Error("a member's intermediate must not be able to sign fleet-wide names")
	}

	if got := s.Fleets(); len(got) != 1 || got[0] != "lab" {
		t.Errorf("Fleets() = %v", got)
	}
	if err := s.RemoveFleet("lab"); err != nil {
		t.Fatal(err)
	}
	if len(s.Fleets()) != 0 || len(s.Clusters()) != 1 {
		t.Errorf("RemoveFleet must remove the fleet only: fleets=%v clusters=%v", s.Fleets(), s.Clusters())
	}
}

// TestConcurrentFleetIssuance mirrors a fleet applied with maxParallel > 1:
// every member asks for the same fleet material at once and must get the same.
func TestConcurrentFleetIssuance(t *testing.T) {
	s := newStore(t)
	const n = 8
	got := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f, err := s.EnsureFleet("lab")
			if err == nil {
				got[i] = f.WildcardPEM
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil || got[i] != got[0] {
			t.Fatalf("member %d: err=%v, same material=%v", i, errs[i], got[i] == got[0])
		}
	}
}
