package peer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRenewedCertificateValidation(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	sign := func(c, issuer *x509.Certificate, pub ed25519.PublicKey) *x509.Certificate {
		t.Helper()
		b, err := x509.CreateCertificate(rand.Reader, c, issuer, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(b)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	root = sign(root, root, pub)
	roots := x509.NewCertPool()
	roots.AddCert(root)
	nodePub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	uri, _ := url.Parse("urn:host-status-mcp:node:" + id)
	seeds, err := asn1.MarshalWithParams("peer.test:7443", "utf8")
	if err != nil {
		t.Fatal(err)
	}
	c := x509.Certificate{SerialNumber: big.NewInt(2), URIs: []*url.URL{uri}, DNSNames: []string{"node.test"}, ExtraExtensions: []pkix.Extension{{Id: seedExtensionOID, Value: seeds}}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Minute)}
	old := sign(&c, root, nodePub)
	s := PeerTLS{id: id, validUntil: old.NotAfter, config: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{{Leaf: old}}}}
	c.NotAfter = time.Now().Add(10 * time.Minute)
	for _, tc := range []struct {
		name   string
		change func(*x509.Certificate)
		key    ed25519.PublicKey
	}{
		{"valid", nil, nodePub},
		{"different key", nil, pub},
		{"foreign identity", func(c *x509.Certificate) {
			u, _ := url.Parse("urn:host-status-mcp:node:" + uuid.NewString())
			c.URIs = []*url.URL{u}
		}, nodePub},
		{"short validity", func(c *x509.Certificate) { c.NotAfter = old.NotAfter }, nodePub},
		{"future validity", func(c *x509.Certificate) { c.NotBefore = time.Now().Add(time.Minute) }, nodePub},
		{"lost server usage", func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} }, nodePub},
		{"lost seed extension", func(c *x509.Certificate) { c.ExtraExtensions = nil }, nodePub},
		{"lost DNS SAN", func(c *x509.Certificate) { c.DNSNames = nil }, nodePub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := c
			if tc.change != nil {
				tc.change(&next)
			}
			cert := sign(&next, root, tc.key)
			_, err := s.renewedChain([]*x509.Certificate{cert, root})
			if (err == nil) != (tc.name == "valid") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestRenewalPathBounds(t *testing.T) {
	ids := make([]string, maxRenewalHops+1)
	for i := range ids {
		ids[i] = uuid.NewString()
	}
	if !validRenewalPath(ids[:maxRenewalHops]) || validRenewalPath(ids) || validRenewalPath([]string{ids[0], ids[1], ids[0]}) || validRenewalPath([]string{"127.0.0.1:443"}) {
		t.Fatal("invalid renewal route acceptance")
	}
}
