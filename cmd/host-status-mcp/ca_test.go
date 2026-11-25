package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/authority"
	"github.com/TakashiSasaki/host-status-mcp/internal/peer"
	"github.com/google/uuid"
	"github.com/smallstep/pkcs7"
)

func caCmd(t *testing.T, ok bool, args ...string) []byte {
	t.Helper()
	cmd := appCommand(t.Context(), append([]string{"ca"}, args...)...)
	out, err := cmd.CombinedOutput()
	if (err == nil) != ok {
		t.Fatalf("ca %s: %v: %s", args[0], err, out)
	}
	return out
}

func fixtureAuthority(t *testing.T, dir string, root testCA, listen string) string {
	t.Helper()
	fixtureESTFiles(t, dir, root)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: "issuing intermediate"}, SerialNumber: big.NewInt(17), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, root.cert, pub, root.key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("issuer.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	der, err = x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write("issuer.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	cfg := authority.Config{Listen: listen, Roots: "root.pem", IssuerCert: "issuer.pem", IssuerKey: "issuer.key", ServerCert: "server.pem", ServerKey: "server.key", StateDir: "state", Lifetime: "50m"}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("ca.json", b)
	path := filepath.Join(dir, "ca.json")
	caCmd(t, true, "init", "-config", path)
	return path
}

func nodeCSR(t *testing.T, c peerConfig, id string) []byte {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse("urn:host-status-mcp:node:" + id)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}, DNSNames: []string{"unapproved.example"}}, pair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func enrollmentArgs(t *testing.T, config string, c peerConfig, id string, dns, seeds []string) []string {
	t.Helper()
	dir := t.TempDir()
	profile, err := json.Marshal(authority.Profile{NodeID: id, DNS: dns, IPs: []string{"127.0.0.1"}, Seeds: seeds})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "profile"), profile, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "csr"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: nodeCSR(t, c, id)}), 0600); err != nil {
		t.Fatal(err)
	}
	return []string{"enroll", "-config", config, "-profile", filepath.Join(dir, "profile"), "-csr", filepath.Join(dir, "csr")}
}
func approvePeer(t *testing.T, config string, c peerConfig, id string, dns, seeds []string) []byte {
	t.Helper()
	return caCmd(t, true, enrollmentArgs(t, config, c, id, dns, seeds)...)
}

func startAuthority(t *testing.T, config string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := appCommand(ctx, "ca", "serve", "-config", config)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	ready := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(stderr)
		if s.Scan() {
			ready <- s.Text()
		}
		for s.Scan() {
		}
	}()
	select {
	case line := <-ready:
		addr, ok := strings.CutPrefix(line, "CA listening on ")
		if !ok {
			t.Fatalf("CA startup: %s", line)
		}
		return "https://" + addr + estPath, stop
	case <-done:
		t.Fatal("CA exited")
	case <-time.After(8 * time.Second):
		t.Fatal("CA startup timed out")
	}
	return "", nil
}

func TestAuthorityLifecycle(t *testing.T) {
	root := newTestCA(t)
	dir := t.TempDir()
	cfg := fixtureAuthority(t, dir, root, "127.0.0.1:0")
	caCmd(t, false, "init", "-config", cfg)
	id := uuid.NewString()
	old := root.issue(t, id, time.Now().Add(4*time.Minute))
	replacement := root.issue(t, id, time.Now().Add(4*time.Minute))
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(readFile(t, root.path))
	url, stop := startAuthority(t, cfg)
	post := func(identity peerConfig, der []byte, want int) []byte {
		t.Helper()
		pair, err := tls.LoadX509KeyPair(identity.CertFile, identity.KeyFile)
		if err != nil {
			t.Fatal(err)
		}
		tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "est.example.invalid", Certificates: []tls.Certificate{pair}}, DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		req, err := http.NewRequestWithContext(t.Context(), "POST", url, strings.NewReader(base64.StdEncoding.EncodeToString(der)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/pkcs10")
		resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("got %d, want %d: %s (%v)", resp.StatusCode, want, b, err)
		}
		return b
	}
	csr := nodeCSR(t, old, id)
	post(old, csr, 403)
	profileSeeds := []string{"b.test:7443", "c.test:7443"}
	initial := approvePeer(t, cfg, old, id, []string{"p.test"}, profileSeeds)
	block, chain := pem.Decode(initial)
	if block == nil || len(chain) == 0 {
		t.Fatal("missing enrolled leaf and intermediate")
	}
	der, err := base64.StdEncoding.DecodeString(string(post(old, csr, 200)))
	if err != nil {
		t.Fatal(err)
	}
	cms, err := pkcs7.Parse(der)
	if err != nil || len(cms.Certificates) != 2 {
		t.Fatalf("invalid issued chain: %v", err)
	}
	for _, c := range cms.Certificates {
		if !c.IsCA && (len(c.DNSNames) != 1 || c.DNSNames[0] != "p.test" || !bytes.Equal(c.RawSubjectPublicKeyInfo, mustLeaf(t, initial).RawSubjectPublicKeyInfo)) {
			t.Fatal("renewal exceeded approved profile")
		}
	}
	tmp := filepath.Join(dir, "enrolled.pem")
	if err = os.WriteFile(tmp, initial, 0600); err != nil {
		t.Fatal(err)
	}
	sec, err := peer.LoadPeerTLS(root.path, tmp, old.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	seeds, err := sec.BootstrapSeeds()
	if err != nil || strings.Join(seeds, ",") != strings.Join(profileSeeds, ",") {
		t.Fatal("seed extension not preserved")
	}
	caCmd(t, false, enrollmentArgs(t, cfg, replacement, id, nil, nil)...)
	post(old, nodeCSR(t, replacement, id), 403)
	post(old, nodeCSR(t, old, uuid.NewString()), 403)
	bad := bytes.Clone(csr)
	bad[len(bad)-1] ^= 1
	post(old, bad, 400)
	caCmd(t, true, "revoke", "-config", cfg, "-node-id", id)
	post(old, csr, 403)
	anotherSerial := old
	anotherSerial.CertFile = tmp
	post(anotherSerial, csr, 403)
	stop()
	url, _ = startAuthority(t, cfg)
	post(old, csr, 403)
	caCmd(t, false, enrollmentArgs(t, cfg, old, id, nil, nil)...)
	approvePeer(t, cfg, replacement, id, nil, nil)
	caCmd(t, true, "revoke", "-config", cfg, "-cert", tmp)
	post(replacement, nodeCSR(t, replacement, id), 200)
	post(old, csr, 403)
}

func mustLeaf(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(data)
	if b == nil {
		t.Fatal("missing certificate PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAuthorityConfig(t *testing.T) {
	root := newTestCA(t)
	dir := t.TempDir()
	cfgPath := fixtureAuthority(t, dir, root, "127.0.0.1:0")
	caCmd(t, true, "config", "check", "-config", cfgPath)
	example := caCmd(t, true, "config", "example")
	var cfg authority.Config
	if err := json.Unmarshal(example, &cfg); err != nil || cfg.Listen != "127.0.0.1:9443" {
		t.Fatalf("invalid example %v", err)
	}
	if err := json.Unmarshal(readFile(t, cfgPath), &cfg); err != nil {
		t.Fatal(err)
	}
	noState := filepath.Join(dir, "uninitialized")
	cfg.StateDir = noState
	write := func() {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(cfgPath, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	caCmd(t, true, "config", "check", "-config", cfgPath)
	if _, err := os.Stat(noState); !os.IsNotExist(err) {
		t.Fatal("config check created CA state")
	}
	caCmd(t, false, "serve", "-config", cfgPath)
	for _, ip := range []string{":9443", "0.0.0.0:9443", "192.0.2.9:9443"} {
		cfg.Listen = ip
		write()
		caCmd(t, false, "config", "check", "-config", cfgPath)
	}
	cfg.Listen = "127.0.0.1:0"
	key, err := x509.MarshalPKCS8PrivateKey(root.key)
	if err != nil {
		t.Fatal(err)
	}
	cfg.IssuerKey = writeTestPEM(t, "PRIVATE KEY", key)
	cfg.IssuerCert = root.path
	write()
	caCmd(t, false, "config", "check", "-config", cfgPath)
	cfg.IssuerKey = "issuer.key"
	cfg.IssuerCert = "issuer.pem"
	p := root.issue(t, uuid.NewString(), time.Now().Add(time.Hour), "ca.example.internal")
	cfg.ServerCert = p.CertFile
	cfg.ServerKey = p.KeyFile
	write()
	caCmd(t, false, "config", "check", "-config", cfgPath)
}
