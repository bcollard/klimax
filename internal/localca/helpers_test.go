package localca

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
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
	c, err := pki.SignLeaf(key.Public(), pkix.Name{CommonName: name}, pki.SANs{DNS: []string{name}}, pki.ServerLeaf, inter, interKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// signWildcardExpiring writes a PEM wildcard (leaf + intermediate) valid for
// only `left` more, signed by the cluster's current intermediate. The wildcard
// key on disk is reused so the pair stays consistent.
func signWildcardExpiring(t *testing.T, s *Store, cluster string, left time.Duration) []byte {
	t.Helper()
	dir := s.clusterDir(cluster)
	inter, interKey := clusterCA(t, s, cluster)
	leafKey, err := pki.LoadKey(filepath.Join(dir, "private", "wildcard.key"))
	if err != nil {
		t.Fatal(err)
	}
	zone := cluster + "." + s.Domain
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		Subject:      pkix.Name{CommonName: "*." + zone},
		DNSNames:     []string{"*." + zone, zone},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(left),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, inter, leafKey.Public(), interKey)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: inter.Raw})...)
}

func asInvalid(err error, target *x509.CertificateInvalidError) bool {
	return errors.As(err, target)
}
