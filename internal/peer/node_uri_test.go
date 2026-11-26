package peer

import (
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/product"
)

func TestNodeURINamespaces(t *testing.T) {
	id := "7043f65e-b1f6-489d-a0d2-5d402035735d"
	var uris []*url.URL
	for _, name := range []string{product.Name, product.LegacyName} {
		u := &url.URL{Scheme: "urn", Opaque: name + ":node:" + id}
		c := &x509.Certificate{URIs: []*url.URL{u}}
		if got, err := CertificateNodeID(c); err != nil || got != id || !HasNodeURI(c) {
			t.Fatalf("expected node identity for %s", u)
		}
		uris = append(uris, u)
		malformed := &x509.Certificate{URIs: []*url.URL{{Scheme: "urn", Opaque: name + ":node:"}}}
		if _, err := CertificateNodeID(malformed); err == nil || !HasNodeURI(malformed) {
			t.Fatal("malformed node identity accepted as peer or EST server")
		}
	}
	if _, err := CertificateNodeID(&x509.Certificate{URIs: uris}); err == nil {
		t.Fatal("ambiguous node identity accepted")
	}
}
