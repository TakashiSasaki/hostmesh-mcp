package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPeerCSR(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	generate := func() ([]byte, error) { return appCommand(t.Context(), "peer", "csr", "-dir", dir).CombinedOutput() }
	output, err := generate()
	if err != nil || bytes.Contains(output, []byte("BEGIN PRIVATE KEY")) {
		t.Fatalf("peer csr failed or printed a private key: %v", err)
	}
	keyFile, csrFile := filepath.Join(dir, "node.key"), filepath.Join(dir, "node.csr")
	key, csr := readFile(t, keyFile), readFile(t, csrFile)
	if _, err := generate(); err == nil {
		t.Fatal("accepted existing credentials directory")
	}
	if !bytes.Equal(key, readFile(t, keyFile)) || !bytes.Equal(csr, readFile(t, csrFile)) {
		t.Fatal("existing credentials changed")
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{dir, keyFile} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm()&0077 != 0 {
				t.Fatalf("expected private %s: %v, %v", path, info, err)
			}
		}
	}

	root := newTestCA(t)
	ca := fixtureAuthority(t, t.TempDir(), root, "127.0.0.1:0")
	id := uuid.NewString()
	profilePath := filepath.Join(t.TempDir(), "node.json")
	profile, err := json.Marshal(authority.Profile{NodeID: id, ClientOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, profile, 0600); err != nil {
		t.Fatal(err)
	}
	chain := caCmd(t, true, "enroll", "-config", ca, "-profile", profilePath, "-csr", csrFile)
	if _, err := tls.X509KeyPair(chain, key); err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "node-issued.pem")
	if err := os.WriteFile(certFile, chain, 0600); err != nil {
		t.Fatal(err)
	}
	listener := root.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	listener.Listen = "127.0.0.1:0"
	hub := startServeProcess(t, config{Peer: listener, MCPListen: "127.0.0.1:0"})
	startServeProcess(t, config{Peer: peerConfig{CAFile: root.path, CertFile: certFile, KeyFile: keyFile, Seeds: []string{hub.peer}}})
	client := mcp.NewClient(&mcp.Implementation{Name: "enrollment-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: hub.mcp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	checkUpgradePeer(t, session, id, strings.TrimSpace(versionFile))
}
