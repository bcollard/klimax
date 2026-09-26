// Package localca is klimax's certificate authority for the local DNS zone
// (network.dns.tls): a root on the Mac, an intermediate per cluster, and a
// wildcard certificate per cluster.
//
// The certificate work is homepki's pkg/pki (github.com/bcollard/homepki),
// linked in-process — no homepki or openssl binary is needed. This package only
// decides the layout, names and constraints.
//
//	~/.klimax/pki/<domain>/
//	├── root.crt                        name-constrained to .<domain>
//	├── private/root.key                never leaves the Mac
//	└── clusters/<cluster>/
//	    ├── ca.crt                      intermediate, constrained to .<cluster>.<domain>
//	    ├── chain.crt                   intermediate + root
//	    ├── private/ca.key
//	    ├── wildcard.crt                *.<cluster>.<domain> + <cluster>.<domain>, then the intermediate
//	    └── private/wildcard.key
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
	"time"

	"github.com/bcollard/homepki/pkg/pki"
)

// keyType is ECDSA P-256 for every tier: small, fast, and accepted everywhere a
// browser or Go client looks. homepki defaults to RSA-2048.
const keyType = "ecdsa"

// renewBefore is how close to expiry a wildcard certificate is re-issued. Leaves
// live pki.LeafValidityDays (365); CAs live pki.CAValidityDays (~6 years).
const renewBefore = 30 * 24 * time.Hour

// Store is the CA for one DNS domain.
type Store struct {
	Dir    string // ~/.klimax/pki/<domain>
	Domain string
}

// New returns the store for domain under klimaxHome. Nothing is created until
// an Ensure call.
func New(klimaxHome, domain string) *Store {
	return &Store{Dir: filepath.Join(klimaxHome, "pki", domain), Domain: domain}
}

// RootCertPath is the root certificate — the file to trust.
func (s *Store) RootCertPath() string { return filepath.Join(s.Dir, "root.crt") }
func (s *Store) rootKeyPath() string  { return filepath.Join(s.Dir, "private", "root.key") }

func (s *Store) clusterDir(cluster string) string { return filepath.Join(s.Dir, "clusters", cluster) }

// Cluster holds one cluster's CA material, PEM-encoded for handing to the guest.
type Cluster struct {
	Name string
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

	nc, err := pki.ParseNameConstraints([]string{"permitted;DNS:." + s.Domain})
	if err != nil {
		return nil, nil, false, err
	}
	if key, err = pki.GenerateKey(keyType); err != nil {
		return nil, nil, false, err
	}
	subject := pkix.Name{Organization: []string{"klimax"}, CommonName: "klimax local CA (" + s.Domain + ")"}
	if cert, err = pki.SelfSignRoot(key, subject, nc); err != nil {
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
	root, rootKey, _, err := s.EnsureRoot()
	if err != nil {
		return nil, err
	}
	zone := cluster + "." + s.Domain
	dir := s.clusterDir(cluster)
	caCrt, caKey := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "private", "ca.key")

	inter, interKey, err := loadPair(caCrt, caKey)
	if err != nil || verifyIntermediate(inter, root) != nil || !constrainedTo(inter, "."+zone) {
		nc, err := pki.ParseNameConstraints([]string{"permitted;DNS:." + zone, "permitted;DNS:" + zone})
		if err != nil {
			return nil, err
		}
		if interKey, err = pki.GenerateKey(keyType); err != nil {
			return nil, err
		}
		subject := pkix.Name{Organization: []string{"klimax"}, OrganizationalUnit: []string{cluster}, CommonName: "klimax " + cluster + " CA"}
		if inter, err = pki.SignIntermediate(interKey.Public(), subject, nc, root, rootKey); err != nil {
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
		subject := pkix.Name{Organization: []string{"klimax"}, OrganizationalUnit: []string{cluster}, CommonName: names[0]}
		if leaf, err = pki.SignLeaf(leafKey.Public(), subject, pki.SANs{DNS: names}, pki.ServerLeaf, inter, interKey); err != nil {
			return nil, err
		}
		if err := s.writePair(wcCrt, wcKey, leafKey, leaf, inter); err != nil {
			return nil, err
		}
	}

	return &Cluster{
		Name:             cluster,
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
func (s *Store) RemoveCluster(cluster string) error {
	if strings.ContainsAny(cluster, "/\\") || cluster == "" || cluster == "." || cluster == ".." {
		return fmt.Errorf("invalid cluster name %q", cluster)
	}
	return os.RemoveAll(s.clusterDir(cluster))
}

// Clusters lists the clusters that have CA material.
func (s *Store) Clusters() []string {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "clusters"))
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

// verifyIntermediate checks that inter was signed by root. pki.VerifyChain needs
// a leaf, so the intermediate is verified as the end of its own chain.
func verifyIntermediate(inter, root *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(root)
	_, err := inter.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
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

// LoadClusterWildcard reads a cluster's wildcard certificate without issuing
// anything.
func LoadClusterWildcard(s *Store, cluster string) (*x509.Certificate, error) {
	return pki.LoadCert(filepath.Join(s.clusterDir(cluster), "wildcard.crt"))
}
