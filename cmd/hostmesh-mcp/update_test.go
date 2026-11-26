//go:build !windows

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/product"
	"github.com/google/uuid"
)

type releaseFixture struct {
	dir    string
	key    ed25519.PrivateKey
	config updateSettings
}

func testReleaseFeed(t *testing.T) releaseFixture {
	t.Helper()
	dir := t.TempDir()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "release.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: raw}), 0600); err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(s.Close)
	return releaseFixture{dir: dir, key: key, config: updateSettings{PublicKeyFile: keyPath, ManifestURL: s.URL + "/release.json", Unit: "hostmesh-mcp-test.service", UserService: true}}
}

func (f releaseFixture) publish(t *testing.T, version string, binary []byte) {
	t.Helper()
	f.publishProduct(t, version, binary, product.Name)
}

func (f releaseFixture) publishProduct(t *testing.T, version string, binary []byte, name string) {
	t.Helper()
	sum := sha256.Sum256(binary)
	artifact := releaseArtifact{URL: "peer", Size: int64(len(binary)), SHA256: hex.EncodeToString(sum[:])}
	m := releaseManifest{Product: name, Version: version, Artifacts: map[string]releaseArtifact{runtime.GOOS + "-" + runtime.GOARCH: artifact}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"peer": binary, "release.json": b, "release.json.sig": ed25519.Sign(f.key, b)} {
		if err := os.WriteFile(filepath.Join(f.dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestManifestIdentity(t *testing.T) {
	f := testReleaseFeed(t)
	for _, name := range []string{product.Name, product.LegacyName, "another-product"} {
		f.publishProduct(t, "0.0.35", []byte("signed fixture"), name)
		_, _, err := loadRelease(t.Context(), f.config)
		if (err != nil) != (name == "another-product") {
			t.Fatalf("unexpected result for signed product %q: %v", name, err)
		}
	}
}

func TestSignedRelease(t *testing.T) {
	f := testReleaseFeed(t)
	f.publish(t, "0.0.18", []byte("signed binary"))
	v, a, err := loadRelease(t.Context(), f.config)
	if err != nil || v != "0.0.18" {
		t.Fatalf("%s %v", v, err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "peer"), []byte("wrong binary!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := stageRelease(t.Context(), filepath.Join(t.TempDir(), "stage"), a, 0700); err == nil {
		t.Fatal("altered binary accepted")
	}
	if err := os.WriteFile(filepath.Join(f.dir, "release.json.sig"), make([]byte, 64), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRelease(t.Context(), f.config); err == nil {
		t.Fatal("unsigned release accepted")
	}
	for _, tc := range []struct {
		old, next string
		want      bool
	}{
		{"0.0.9", "0.0.10", true}, {"0.1.0", "0.0.10", false}, {"0.0.18", "0.0.18", false},
	} {
		got, err := newerRelease(tc.old, tc.next)
		if err != nil || got != tc.want {
			t.Fatalf("%s -> %s: %t %v", tc.old, tc.next, got, err)
		}
	}
	for _, bad := range []string{"latest", "0.0.18-rc1", "0.0.018", "0.0.18/../../x"} {
		if _, err := releaseVersion(bad); err == nil {
			t.Fatalf("invalid stable version %s", bad)
		}
	}
}

// Run with a real, isolated user unit. Both inputs are standalone builds;
// the old one must provide server_config and have a lower version.
func TestSystemdUpdate(t *testing.T) {
	if os.Getenv("HOSTMESH_MCP_UPDATE_SYSTEMD_TEST") != "1" {
		t.Skip("opt-in user systemd integration")
	}
	old, next := os.Getenv("HOSTMESH_MCP_PREVIOUS_BINARY"), os.Getenv("HOSTMESH_MCP_TEST_BINARY")
	work := t.TempDir()
	f := testReleaseFeed(t)
	c := f.config
	c.Binary = filepath.Join(work, "hostmesh-mcp")
	c.Unit = "hostmesh-mcp-test-" + uuid.NewString() + ".service"
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(work, name)
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	asJSON := func(v any) []byte {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	write("hostmesh-mcp", read(old), 0700)
	vOld, err := commandOutput(t.Context(), "", old, "version")
	if err != nil {
		t.Fatal(err)
	}
	vNext, err := commandOutput(t.Context(), "", next, "version")
	if err != nil {
		t.Fatal(err)
	}
	ca := oldFormatTestCA(t)
	id := uuid.NewString()
	p := ca.issue(t, id, time.Now().Add(time.Hour))
	hub := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	hub.Listen = "127.0.0.1:0"
	observer := startServeProcess(t, config{MCPListen: "127.0.0.1:0", Peer: hub})
	p.Seeds = []string{observer.peer}
	c.Observer = observer.mcp
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	stateDir := filepath.Join(work, "ledger")
	peerPath := write("peer.json", asJSON(config{StateDir: stateDir, MCPListen: addr, Peer: p}), 0600)
	settings := write("updater.json", asJSON(c), 0600)
	unit := write(c.Unit, []byte(fmt.Sprintf("[Unit]\nDescription=Disposable hostmesh-mcp update test\n[Service]\nWorkingDirectory=%s\nExecStart=%s serve -config %s\n", work, c.Binary, peerPath)), 0644)
	ctl := func(ctx context.Context, args ...string) error {
		out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w", out, err)
		}
		return nil
	}
	if err := ctl(t.Context(), "link", "--runtime", unit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ctl(ctx, "stop", c.Unit); err != nil {
			t.Error(err)
		}
		if err := ctl(ctx, "disable", "--runtime", c.Unit); err != nil {
			t.Error(err)
		}
	})
	target := updateTarget{Unit: c.Unit, User: true, StateDir: stateDir, NodeID: id, Endpoint: "http://" + addr + "/mcp", Observer: c.Observer, Previous: vOld}
	if err := target.startAndVerify(t.Context(), target, c.Binary, vOld, false); err != nil {
		t.Fatal(err)
	}
	update := func(action string) (string, error) {
		return commandOutput(t.Context(), work, next, "update", action, "-config", settings)
	}
	f.publish(t, vNext, read(next))
	if _, err := update("check"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "peer"), []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := update("apply"); err == nil {
		t.Fatal("accepted tampered binary")
	}
	if err := target.probe(t.Context(), vOld); err != nil {
		t.Fatal(err)
	}
	f.publish(t, vNext, read(next))
	write("peer.json", asJSON(config{StateDir: filepath.Join(work, "other-ledger"), MCPListen: addr, Peer: p}), 0600)
	if _, err := update("apply"); err == nil || !strings.Contains(err.Error(), "state_dir differs") {
		t.Fatalf("changed state directory was not refused before downtime: %v", err)
	}
	write("peer.json", asJSON(config{StateDir: stateDir, MCPListen: addr, Peer: p}), 0600)
	// Signed test fixture passes preflight but exits during serve.
	broken := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nversion) echo %s;;\nconfig) exit 0;;\n*) printf invalid > ledger/state.json; exit 42;;\nesac\n", vNext)
	f.publish(t, vNext, []byte(broken))
	if out, err := update("apply"); err == nil || !strings.Contains(err.Error(), "restored "+vOld) {
		t.Fatalf("rollback: %s %v", out, err)
	}
	if err := target.probe(t.Context(), vOld); err != nil {
		t.Fatal(err)
	}
	// Terminate the updater between installation and acceptance. A concurrent
	// apply must be rejected; the next apply must recover even with a bad feed.
	waiting := strings.Replace(broken, "exit 42", "exec /bin/sleep 90", 1)
	f.publish(t, vNext, []byte(waiting))
	child := exec.CommandContext(t.Context(), next, "update", "apply", "-config", settings)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil {
			child.Process.Kill()
			child.Wait()
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(c.Binary); err == nil && string(data) == waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("candidate not installed before interruption")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := update("apply"); err == nil || !strings.Contains(err.Error(), ".update-lock") {
		t.Fatalf("concurrent apply: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	if err := os.WriteFile(filepath.Join(f.dir, "release.json.sig"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := update("apply"); err != nil || !strings.Contains(out, "Recovered previous service") {
		t.Fatalf("interrupted recovery: %s %v", out, err)
	}
	if err := target.probe(t.Context(), vOld); err != nil {
		t.Fatal(err)
	}
	f.publish(t, vNext, read(next))
	if _, err := update("apply"); err != nil {
		t.Fatal(err)
	}
	if err := target.probe(t.Context(), vNext); err != nil {
		t.Fatal(err)
	}
	installed, err := os.Stat(c.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Mode().Perm() != 0700 {
		t.Fatalf("updated executable permissions: %v", installed.Mode())
	}
	before, err := target.processDir(t.Context(), c.Binary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := update("apply"); err != nil {
		t.Fatal(err)
	}
	after, err := target.processDir(t.Context(), c.Binary)
	if err != nil || before != after {
		t.Fatalf("same-version apply restarted service: %s -> %s (%v)", before, after, err)
	}
	// Outbound-only peers use the independent observer for both probes.
	if err := ctl(t.Context(), "stop", c.Unit); err != nil {
		t.Fatal(err)
	}
	write("hostmesh-mcp", read(old), 0700)
	write("peer.json", asJSON(config{StateDir: stateDir, Peer: p}), 0600)
	target.Endpoint = ""
	if err := target.startAndVerify(t.Context(), target, c.Binary, vOld, false); err != nil {
		t.Fatal(err)
	}
	if _, err := update("apply"); err != nil {
		t.Fatal(err)
	}
	if err := target.probe(t.Context(), vNext); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "updater.json")
	for _, tc := range []struct {
		config, unit, interval string
		user, valid            bool
	}{
		{"/peer.json", "", "", false, true},
		{"/peer.json", "", "15m", false, true},
		{"relative.json", "", "", false, false},
		{"/peer.json", "peer.service", "", false, false},
		{"/peer.json", "", "", true, false},
		{"/peer.json", "", "0s", false, false},
		{"", "peer.service", "15m", false, false},
		{"", "peer.service", "", true, true},
	} {
		settings := updateSettings{ManifestURL: "https://example.org/release.json", Binary: "/bin/peer", PublicKeyFile: "/release.pub", PeerConfig: tc.config, Unit: tc.unit, UserService: tc.user, CheckInterval: tc.interval}
		b, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
		c, err := readUpdateSettings(path)
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", b, err)
		}
		if err == nil && c.PeerConfig != "" && c.CheckInterval == "" {
			if interval, err := c.interval(); err != nil || interval != 24*time.Hour {
				t.Fatalf("default interval: %s, %v", interval, err)
			}
		}
	}
}

func TestGitHubUpdateSettings(t *testing.T) {
	for _, tc := range []struct {
		source, manifest, key string
		ok                    bool
	}{
		{"github", "", "", true},
		{"github", "https://example.org/release.json", "", false},
		{"github", "", "/release.pub", false},
		{"github-mirror", "", "", false},
		{"manifest", "https://example.org/release.json", "/release.pub", true},
	} {
		cfg := updateSettings{Source: tc.source, ManifestURL: tc.manifest, PublicKeyFile: tc.key, Binary: "/opt/peer", Unit: "peer.service"}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readUpdateSettings(p); (err == nil) != tc.ok {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}
