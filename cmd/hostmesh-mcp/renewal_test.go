package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"

	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/smallstep/pkcs7"
)

const estPath = "/.well-known/est/simplereenroll"
const estURL = "https://est.example.invalid:9443" + estPath

// This test issuer authorizes the authenticated client, independently checks the
// CSR proof and key continuity, and derives the identity from the old leaf.
func fixtureEST(ca testCA, allowed func(*x509.Certificate) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != estPath || r.Method != "POST" || r.Header.Get("Content-Type") != "application/pkcs10" || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "bad request", 400)
			return
		}
		old := r.TLS.VerifiedChains[0][0]
		if !allowed(old) {
			http.Error(w, "revoked", 403)
			return
		}
		der, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, io.LimitReader(r.Body, 8192)))
		if err != nil {
			http.Error(w, "bad encoding", 400)
			return
		}
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil || csr.CheckSignature() != nil || !bytes.Equal(csr.RawSubjectPublicKeyInfo, old.RawSubjectPublicKeyInfo) || len(csr.URIs) != 1 || csr.URIs[0].String() != old.URIs[0].String() {
			http.Error(w, "bad CSR", 400)
			return
		}
		next := *old
		next.NotBefore = time.Now().Add(-time.Minute)
		next.NotAfter = time.Now().Add(50 * time.Minute)
		next.SerialNumber = new(big.Int).Add(old.SerialNumber, big.NewInt(1))
		next.ExtraExtensions = csr.Extensions
		issued, err := x509.CreateCertificate(rand.Reader, &next, ca.cert, csr.PublicKey, ca.key)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		cms, err := pkcs7.DegenerateCertificate(issued)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/pkcs7-mime; smime-type=certs-only")
		fmt.Fprint(w, base64.StdEncoding.EncodeToString(cms))
	})
}

func fixtureESTFiles(t *testing.T, dir string, ca testCA) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	server := &x509.Certificate{SerialNumber: big.NewInt(71), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"est.example.invalid"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, server, ca.cert, pub, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	root := readFile(t, ca.path)
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("root.pem", root)
	write("server.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	der, err = x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write("server.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func estServerTLS(t *testing.T, dir string) *tls.Config {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(readFile(t, filepath.Join(dir, "root.pem"))) {
		t.Fatal("root PEM")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, Certificates: []tls.Certificate{pair}}
}

func TestMeshCertificateRenewal(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	fixtureESTFiles(t, dir, ca)
	allowedID, revokedID := uuid.NewString(), uuid.NewString()
	var refused, issued atomic.Int32
	est := httptest.NewUnstartedServer(fixtureEST(ca, func(c *x509.Certificate) bool {
		if len(c.URIs) != 1 {
			return false
		}
		switch c.URIs[0].Opaque {
		case "host-status-mcp:node:" + allowedID:
			issued.Add(1)
			return true
		case "host-status-mcp:node:" + revokedID:
			refused.Add(1)
		}
		return false
	}))
	est.TLS = estServerTLS(t, dir)
	est.StartTLS()
	defer est.Close()
	// B listens. Both the gateway G and the applicants can only initiate to B.
	mid := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	mid.Listen = "127.0.0.1:0"
	mid.Renewal = &peer.RenewalConfig{Relay: true}
	b := startServeProcess(t, config{Peer: mid})
	gateway := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	gateway.Seeds = []string{b.peer}
	gateway.Renewal = &peer.RenewalConfig{Gateway: est.Listener.Addr().String()}
	startServeProcess(t, config{Peer: gateway})
	applicant := func(id string) (peerConfig, readyProcess) {
		c := ca.issueWithExtensions(t, id, time.Now().Add(3*time.Minute), seedExtension(t, b.peer))
		c.Renewal = &peer.RenewalConfig{URL: estURL}
		return c, startServeProcess(t, config{MCPListen: "127.0.0.1:0", Peer: c})
	}
	revoked, _ := applicant(revokedID)
	original := readFile(t, revoked.CertFile)
	keyData := readFile(t, revoked.KeyFile)
	good, a := applicant(allowedID)
	goodKey := readFile(t, good.KeyFile)
	deadline := time.Now().Add(18 * time.Second)
	for {
		block, _ := pem.Decode(readFile(t, good.CertFile))
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil && time.Until(cert.NotAfter) > 40*time.Minute && refused.Load() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("renewal timeout: issued=%d refused=%d", issued.Load(), refused.Load())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !bytes.Equal(original, readFile(t, revoked.CertFile)) || !bytes.Equal(keyData, readFile(t, revoked.KeyFile)) || !bytes.Equal(goodKey, readFile(t, good.KeyFile)) {
		t.Fatal("changed a revoked certificate or replaced a private key")
	}
	checkRenewedService(t, a.mcp, allowedID, readFile(t, good.CertFile))
}

func checkRenewedService(t *testing.T, endpoint, id string, cert []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		client := mcp.NewClient(&mcp.Implementation{Name: "renewal-probe", Version: "test"}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
		if err == nil {
			var config struct {
				NodeID      string `json:"node_id"`
				Certificate struct {
					PEM string `json:"pem"`
				} `json:"certificate"`
			}
			err = toolResult(ctx, s, "server_config", map[string]any{"include_certificate": true}, &config)
			s.Close()
			block, _ := pem.Decode(cert)
			if err == nil && config.NodeID == id && block != nil && config.Certificate.PEM == string(pem.EncodeToMemory(block)) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("renewed certificate not active on %s for %s", endpoint, id)
}

func TestRenewalConfiguration(t *testing.T) {
	ca := newTestCA(t)
	mtls := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	combined := filepath.Join(t.TempDir(), "node.pem")
	if err := os.WriteFile(combined, append(readFile(t, mtls.CertFile), readFile(t, mtls.KeyFile)...), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		configure func(*peerConfig)
		valid     bool
	}{
		{"mtls applicant", func(p *peerConfig) { p.Renewal = &peer.RenewalConfig{URL: estURL} }, true},
		{"plaintext exit", func(p *peerConfig) { *p = peerConfig{Renewal: &peer.RenewalConfig{Gateway: "127.0.0.1:9443"}} }, false},
		{"insecure CA URL", func(p *peerConfig) {
			p.Renewal = &peer.RenewalConfig{URL: strings.Replace(estURL, "https:", "http:", 1)}
		}, false},
		{"non-EST endpoint", func(p *peerConfig) { p.Renewal = &peer.RenewalConfig{URL: "https://ca.test/sign"} }, false},
		{"same file as key", func(p *peerConfig) {
			p.Renewal = &peer.RenewalConfig{URL: estURL}
			p.CertFile = combined
			p.KeyFile = combined
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := config{MCPListen: "127.0.0.1:0", Peer: mtls}
			tc.configure(&c.Peer)
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "peer.json")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig(path)
			if err == nil {
				_, err = cfg.preparePeering()
			}
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}
