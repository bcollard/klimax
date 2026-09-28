// Package localca is marina's certificate authority for the local DNS zone
// (network.dns.tls): a root on the Mac, an intermediate per cluster, and a
// wildcard certificate per cluster.
//
// The certificate work is homepki's pkg/pki (github.com/bcollard/homepki),
// linked in-process — no homepki or openssl binary is needed. This package only
// decides the layout, names and constraints.
//
//	~/.marina/pki/<domain>/
//	├── root.crt                        name-constrained to .<domain>
//	├── private/root.key                never leaves the Mac
//	├── clusters/<cluster>/
//	│   ├── ca.crt                      intermediate, constrained to .<cluster>.<domain>
//	│   ├── chain.crt                   intermediate + root
//	│   ├── private/ca.key
//	│   ├── wildcard.crt                *.<cluster>.<domain> + <cluster>.<domain>, then the intermediate
//	│   └── private/wildcard.key
//	└── fleets/<fleet>/                 same layout, for the fleet's shared zone
//	                                    (gateway.<fleet>.<domain>), installed into every member
//
// The constraints are the point. The root goes into the System keychain, where
// it would otherwise vouch for any site; constrained, nothing below it is
// accepted outside the zone (Go, macOS, Chrome and Firefox enforce this). The
// intermediate's key goes into the cluster when cert-manager uses it, and a
// leaked one can only sign for its own cluster.
package localca

import (
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bcollard/homepki/pkg/pki"
)

// keyType is ECDSA P-256 for every tier: small, fast, and accepted everywhere a
// browser or Go client looks. homepki defaults to RSA-2048.
const keyType = "ecdsa"

// renewBefore is how close to expiry a wildcard certificate is re-issued. Leaves
// live pki.LeafValidityDays (365); CAs live pki.CAValidityDays (~6 years).
const renewBefore = 30 * 24 * time.Hour

// storeMu serialises issuance. A fleet applied with maxParallel > 1 creates its
// members concurrently, and every member asks for the same fleet intermediate:
// unserialised, two goroutines would each generate one and interleave their
// writes to ca.crt and ca.key. One process-wide lock is enough — issuance is a
// few milliseconds, and marina runs one command at a time.
var storeMu sync.Mutex

// Store is the CA for one DNS domain.
type Store struct {
	Dir    string // ~/.marina/pki/<domain>
	Domain string
}

// New returns the store for domain under marinaHome. Nothing is created until
// an Ensure call.
func New(marinaHome, domain string) *Store {
	return &Store{Dir: filepath.Join(marinaHome, "pki", domain), Domain: domain}
}

// RootCertPath is the root certificate — the file to trust.
func (s *Store) RootCertPath() string { return filepath.Join(s.Dir, "root.crt") }
func (s *Store) rootKeyPath() string  { return filepath.Join(s.Dir, "private", "root.key") }

// Kind selects which zone an intermediate serves.
type Kind string

const (
	// ClusterZone is <cluster>.<domain>: one per cluster.
	ClusterZone Kind = "clusters"
	// FleetZone is <fleet>.<domain>: shared by a fleet's members.
	FleetZone Kind = "fleets"
)

func (s *Store) zoneDir(kind Kind, name string) string {
	return filepath.Join(s.Dir, string(kind), name)
}
func (s *Store) clusterDir(cluster string) string { return s.zoneDir(ClusterZone, cluster) }

// Cluster holds one zone's CA material — a cluster's or a fleet's —
// PEM-encoded for handing to the guest.
type Cluster struct {
	Name string
	Kind Kind
	// ChainPEM is the intermediate followed by the root: what a cert-manager
	// CA issuer's Secret carries as tls.crt.
	ChainPEM string
	// KeyPEM is the intermediate's private key.
	KeyPEM string
	// WildcardPEM is the wildcard leaf followed by the intermediate — what a
	// server presents, so clients that trust only the root can build the path.
	WildcardPEM string
	// WildcardKeyPEM is the wildcard leaf's private key.
	WildcardKeyPEM string
	// RootPEM is the root certificate alone, for trust stores.
	RootPEM string
	// WildcardNames are the names the wildcard is valid for.
	WildcardNames []string
	// WildcardNotAfter is when the wildcard expires.
	WildcardNotAfter time.Time
}

// EnsureRoot loads the root CA, creating it on first use. created reports
// whether it was just made — the caller then needs to trust it.
func (s *Store) EnsureRoot() (cert *x509.Certificate, key crypto.Signer, created bool, err error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	return s.ensureRoot()
}

func (s *Store) ensureRoot() (cert *x509.Certificate, key crypto.Signer, created bool, err error) {
	cert, key, err = s.loadRoot()
	if err == nil {
		if !constrainedTo(cert, "."+s.Domain) {
			return nil, nil, false, fmt.Errorf("%s is not constrained to .%s — move it aside and re-run to create a new root", s.RootCertPath(), s.Domain)
		}
		return cert, key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, err
	}

	nc := pki.NameConstraints{}.PermitDNS("." + s.Domain)
	if key, err = pki.GenerateKey(keyType); err != nil {
		return nil, nil, false, err
	}
	subject := pkix.Name{Organization: []string{"marina"}, CommonName: "marina local CA (" + s.Domain + ")"}
	if cert, err = pki.SelfSignRoot(key, subject, nc, pki.Days(pki.CAValidityDays)); err != nil {
		return nil, nil, false, err
	}
	if err := s.writePair(s.RootCertPath(), s.rootKeyPath(), key, cert); err != nil {
		return nil, nil, false, err
	}
	return cert, key, true, nil
}

func (s *Store) loadRoot() (*x509.Certificate, crypto.Signer, error) {
	if _, err := os.Stat(s.RootCertPath()); err != nil {
		return nil, nil, err
	}
	cert, err := pki.LoadCert(s.RootCertPath())
	if err != nil {
		return nil, nil, err
	}
	key, err := pki.LoadKey(s.rootKeyPath())
	if err != nil {
		return nil, nil, err
	}
	if err := pki.CheckKeyPair(cert, key); err != nil {
		return nil, nil, fmt.Errorf("%s does not match its key: %w", s.RootCertPath(), err)
	}
	return cert, key, nil
}

// EnsureCluster returns a cluster's intermediate and wildcard, creating or
// renewing whatever is missing, expiring, or no longer chains to the current
// root. Idempotent: a second call returns the same material.
func (s *Store) EnsureCluster(cluster string) (*Cluster, error) {
	return s.ensureZone(ClusterZone, cluster)
}

// EnsureFleet is EnsureCluster for a fleet's shared zone. Its intermediate is
// constrained to .<fleet>.<domain>, so it cannot sign for any member's own
// zone — nor a member's intermediate for the fleet's.
func (s *Store) EnsureFleet(fleet string) (*Cluster, error) {
	return s.ensureZone(FleetZone, fleet)
}

func (s *Store) ensureZone(kind Kind, name string) (*Cluster, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	root, rootKey, _, err := s.ensureRoot()
	if err != nil {
		return nil, err
	}
	cluster := name
	zone := name + "." + s.Domain
	dir := s.zoneDir(kind, name)
	caCrt, caKey := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "private", "ca.key")

	inter, interKey, err := loadPair(caCrt, caKey)
	if err != nil || pki.VerifyIntermediate(inter, root) != nil || !constrainedExactly(inter, zone) {
		// One subtree, without a leading dot: RFC 5280 reads "zone" as the zone
		// and everything under it, and so do Go and Chrome. Apple's verifier
		// (Safari, Go on darwin, `security`) instead requires a name to match
		// EVERY permitted DNS subtree: with ".zone" and "zone" both listed, the
		// wildcard's bare "zone" SAN fails ".zone", and the certificate is
		// rejected as a name-constraint violation. v0.2.2–0.2.3 issued exactly
		// that, so an intermediate with any other constraint set is re-issued.
		nc := pki.NameConstraints{}.PermitDNS(zone)
		if interKey, err = pki.GenerateKey(keyType); err != nil {
			return nil, err
		}
		subject := pkix.Name{Organization: []string{"marina"}, OrganizationalUnit: []string{cluster}, CommonName: "marina " + cluster + " CA"}
		if inter, err = pki.SignIntermediate(interKey.Public(), subject, nc, pki.Days(pki.CAValidityDays), root, rootKey); err != nil {
			return nil, err
		}
		if err := s.writePair(caCrt, caKey, interKey, inter); err != nil {
			return nil, err
		}
		// A new intermediate orphans the old wildcard.
		_ = os.Remove(filepath.Join(dir, "wildcard.crt"))
	}
	if err := pki.WriteCerts(filepath.Join(dir, "chain.crt"), inter, root); err != nil {
		return nil, err
	}

	names := []string{"*." + zone, zone}
	wcCrt, wcKey := filepath.Join(dir, "wildcard.crt"), filepath.Join(dir, "private", "wildcard.key")
	leaf, leafKey, err := loadPair(wcCrt, wcKey)
	if err != nil || time.Until(leaf.NotAfter) < renewBefore ||
		pki.VerifyChain(leaf, inter, root, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) != nil {
		if leafKey, err = pki.GenerateKey(keyType); err != nil {
			return nil, err
		}
		subject := pkix.Name{Organization: []string{"marina"}, OrganizationalUnit: []string{cluster}, CommonName: names[0]}
		if leaf, err = pki.SignLeaf(leafKey.Public(), subject, pki.SANs{DNS: names}, pki.ServerLeaf, pki.Days(pki.LeafValidityDays), inter, interKey); err != nil {
			return nil, err
		}
		if err := s.writePair(wcCrt, wcKey, leafKey, leaf, inter); err != nil {
			return nil, err
		}
	}

	return &Cluster{
		Name:             cluster,
		Kind:             kind,
		ChainPEM:         mustRead(filepath.Join(dir, "chain.crt")),
		KeyPEM:           mustRead(caKey),
		WildcardPEM:      mustRead(wcCrt),
		WildcardKeyPEM:   mustRead(wcKey),
		RootPEM:          mustRead(s.RootCertPath()),
		WildcardNames:    names,
		WildcardNotAfter: leaf.NotAfter,
	}, nil
}

// RemoveCluster deletes a cluster's intermediate and wildcard. There is no
// revocation: a local CA has no CRL anyone checks. Deleting the key is what
// stops anything new being signed with it.
func (s *Store) RemoveCluster(cluster string) error { return s.removeZone(ClusterZone, cluster) }

// RemoveFleet deletes a fleet's intermediate and wildcard.
func (s *Store) RemoveFleet(fleet string) error { return s.removeZone(FleetZone, fleet) }

func (s *Store) removeZone(kind Kind, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.zoneDir(kind, name))
}

// validName refuses anything that would escape the store directory.
func validName(name string) error {
	if strings.ContainsAny(name, "/\\") || name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid name %q", name)
	}
	return nil
}

// Clusters lists the clusters that have CA material.
func (s *Store) Clusters() []string { return s.zones(ClusterZone) }

// Fleets lists the fleets that have CA material.
func (s *Store) Fleets() []string { return s.zones(FleetZone) }

func (s *Store) zones(kind Kind) []string {
	entries, err := os.ReadDir(filepath.Join(s.Dir, string(kind)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// Root returns the root certificate if it exists, without creating it.
func (s *Store) Root() (*x509.Certificate, error) {
	return pki.LoadCert(s.RootCertPath())
}

func (s *Store) writePair(certPath, keyPath string, key crypto.Signer, certs ...*x509.Certificate) error {
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	// Tighten an existing private/ too — MkdirAll leaves its mode alone.
	if err := os.Chmod(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	if err := pki.WriteKey(keyPath, key); err != nil {
		return err
	}
	return pki.WriteCerts(certPath, certs...)
}

func loadPair(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	cert, err := pki.LoadCert(certPath)
	if err != nil {
		return nil, nil, err
	}
	key, err := pki.LoadKey(keyPath)
	if err != nil {
		return nil, nil, err
	}
	if err := pki.CheckKeyPair(cert, key); err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// constrainedExactly reports whether a CA certificate's permitted DNS subtrees
// are exactly [want].
func constrainedExactly(c *x509.Certificate, want string) bool {
	return len(c.PermittedDNSDomains) == 1 && strings.EqualFold(c.PermittedDNSDomains[0], want)
}

// constrainedTo reports whether a CA certificate's permitted DNS subtrees
// include want.
func constrainedTo(c *x509.Certificate, want string) bool {
	for _, d := range c.PermittedDNSDomains {
		if strings.EqualFold(d, want) {
			return true
		}
	}
	return false
}

func mustRead(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// LoadWildcard reads a zone's wildcard certificate without issuing anything.
func LoadWildcard(s *Store, kind Kind, name string) (*x509.Certificate, error) {
	return pki.LoadCert(filepath.Join(s.zoneDir(kind, name), "wildcard.crt"))
}
