package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
	"github.com/google/uuid"
	"github.com/smallstep/pkcs7"
)

func TestCABootstrap(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	out := caCmd(t, true, "bootstrap", "-dir", dir, "-server-name", "est.example.test")
	online := filepath.Join(dir, "online")
	config := filepath.Join(online, "ca.json")
	rootPath := filepath.Join(online, "root.pem")
	if !strings.Contains(string(out), config) || bytes.Contains(out, []byte("BEGIN PRIVATE KEY")) {
		t.Fatal("unexpected bootstrap output")
	}
	root := mustLeaf(t, readFile(t, rootPath))
	issuer := mustLeaf(t, readFile(t, filepath.Join(online, "issuer.pem")))
	est := mustLeaf(t, readFile(t, filepath.Join(online, "est-chain.pem")))
	if root.CheckSignatureFrom(root) != nil || root.MaxPathLen != 1 || issuer.CheckSignatureFrom(root) != nil || !issuer.MaxPathLenZero || est.CheckSignatureFrom(issuer) != nil || est.IsCA || est.VerifyHostname("est.example.test") != nil {
		t.Fatal("invalid generated certificate chain")
	}
	if _, err := tls.LoadX509KeyPair(rootPath, filepath.Join(dir, "offline", "root.key")); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, online, filepath.Join(dir, "offline", "root.key"), filepath.Join(online, "issuer.key"), filepath.Join(online, "est.key")} {
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm()&0077 != 0 {
				t.Fatalf("expected private %s: %v %v", path, st, err)
			}
		}
	}
	caCmd(t, false, "bootstrap", "-dir", dir, "-server-name", "changed.test")
	if !bytes.Equal(root.Raw, mustLeaf(t, readFile(t, rootPath)).Raw) {
		t.Fatal("existing CA was replaced")
	}
	if err := os.Rename(filepath.Join(dir, "offline"), filepath.Join(t.TempDir(), "offline")); err != nil {
		t.Fatal(err)
	}
	caCmd(t, true, "config", "check", "-config", config)
	caCmd(t, false, "init", "-config", config)

	// Give the server an ephemeral port and enroll a new peer using its own key.
	var cfg authority.Config
	if err := json.Unmarshal(readFile(t, config), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Listen = "127.0.0.1:0"
	writeConfig := func() {
		t.Helper()
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(config, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	normalLifetime := cfg.Lifetime
	cfg.Lifetime = "1h"
	writeConfig()
	id := uuid.NewString()
	p := newTestCA(t).issue(t, id, time.Now().Add(time.Hour))
	issued := approvePeer(t, config, p, id, []string{"sensor.test"}, nil)
	// Return to the default validity so immediate reenrollment extends the leaf.
	cfg.Lifetime = normalLifetime
	writeConfig()
	initial := mustLeaf(t, issued)
	clientPair, err := tls.X509KeyPair(issued, readFile(t, p.KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "est.example.test", Certificates: []tls.Certificate{clientPair}}}
	defer tr.CloseIdleConnections()
	endpoint, _ := startAuthority(t, config)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(base64.StdEncoding.EncodeToString(nodeCSR(t, p, id))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/pkcs10")
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.Status)
	}
	der, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, resp.Body))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := pkcs7.Parse(der)
	if err != nil {
		t.Fatal(err)
	}
	for _, cert := range msg.Certificates {
		if cert.IsCA {
			continue
		}
		if cert.CheckSignatureFrom(issuer) == nil && cert.VerifyHostname("sensor.test") == nil &&
			bytes.Equal(cert.RawSubjectPublicKeyInfo, initial.RawSubjectPublicKeyInfo) && cert.NotAfter.After(initial.NotAfter) {
			return
		}
	}
	t.Fatal("expected a renewed peer certificate")
}

func TestCABootstrapServerName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	for _, bad := range []string{"", "https://ca.test", "ca..test", "ca.test:9443"} {
		caCmd(t, false, "bootstrap", "-dir", dir, "-server-name", bad)
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid name created an output directory: %v", err)
		}
	}
	caCmd(t, true, "bootstrap", "-dir", dir, "-server-name", "::1")
	est := mustLeaf(t, readFile(t, filepath.Join(dir, "online", "est-chain.pem")))
	if err := est.VerifyHostname("::1"); err != nil {
		t.Fatal(err)
	}
}
