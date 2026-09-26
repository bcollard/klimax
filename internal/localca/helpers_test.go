package localca

import (
	"crypto"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcollard/homepki/pkg/pki"
)

// clusterCA loads a cluster's intermediate and key from disk.
func clusterCA(t *testing.T, s *Store, cluster string) (*x509.Certificate, crypto.Signer) {
	t.Helper()
	dir := s.clusterDir(cluster)
	inter, key, err := loadPair(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "private", "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	return inter, key
}

// signWith signs a leaf for name with the cluster's intermediate — what someone
// holding the intermediate's key (from the cluster Secret) could do.
func signWith(t *testing.T, s *Store, cluster, name string) *x509.Certificate {
	t.Helper()
	inter, interKey := clusterCA(t, s, cluster)
	key, err := pki.GenerateKey("ecdsa")
	if err != nil {
		t.Fatal(err)
	}
	c, err := pki.SignLeaf(key.Public(), pkix.Name{CommonName: name}, pki.SANs{DNS: []string{name}}, pki.ServerLeaf, 0, inter, interKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signWildcardExpiring writes a PEM wildcard (leaf + intermediate) valid for
// only `left`, signed by the cluster's current intermediate. The wildcard key
// on disk is reused so the pair stays consistent.
func signWildcardExpiring(t *testing.T, s *Store, cluster string, left time.Duration) []byte {
	t.Helper()
	inter, interKey := clusterCA(t, s, cluster)
	leafKey, err := pki.LoadKey(filepath.Join(s.clusterDir(cluster), "private", "wildcard.key"))
	if err != nil {
		t.Fatal(err)
	}
	zone := cluster + "." + s.Domain
	leaf, err := pki.SignLeaf(leafKey.Public(), pkix.Name{CommonName: "*." + zone},
		pki.SANs{DNS: []string{"*." + zone, zone}}, pki.ServerLeaf, left, inter, interKey)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: inter.Raw})...)
}

func asInvalid(err error, target *x509.CertificateInvalidError) bool {
	return errors.As(err, target)
}

func writePEM(t *testing.T, path string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}
