//go:build linux

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDockerRelay(t *testing.T)     { testDockerRelay(t, false, false) }
func TestDockerMTLSRelay(t *testing.T) { testDockerRelay(t, true, false) }

func TestDockerMeshCertificateRenewal(t *testing.T) { testDockerRelay(t, true, true) }

func testDockerRelay(t *testing.T, mtls, renewal bool) {
	if os.Getenv("HOST_STATUS_DOCKER_TEST") != "1" {
		t.Skip("set HOST_STATUS_DOCKER_TEST=1 to use the local Docker daemon")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	imageDir := filepath.Join(root, "image")
	if err := os.Mkdir(imageDir, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	build := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %v: %v\n%s", args, err, b)
		}
	}

	build("build", "-o", filepath.Join(imageDir, "hostmesh-mcp"), ".")
	build("test", "-c", "-o", filepath.Join(imageDir, "probe"), ".")
	rootfs := filepath.Join(root, "rootfs.tar")
	// Give only B a different timezone so the response identifies the sampled node.
	zones, err := zip.OpenReader(filepath.Join(runtime.GOROOT(), "lib", "time", "zoneinfo.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer zones.Close()
	zone, err := fs.ReadFile(zones, "Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "tokyo.zone"), zone, 0644); err != nil {
		t.Fatal(err)
	}
	run("tar", "-C", imageDir, "-cf", rootfs, "hostmesh-mcp", "probe", "tokyo.zone")
	osRelease := filepath.Join(root, "test-os-release")
	if err := os.WriteFile(osRelease, []byte("ID=lab-linux\nNAME=\"Lab Linux\"\nVERSION_ID=2.4\nVERSION=\"2.4 (Test)\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	prefix := "host-status-lab-" + uuid.NewString()[:12]
	image, left, right := prefix+":test", prefix+"-left", prefix+"-right"
	a, b, c := prefix+"-a", prefix+"-b", prefix+"-c"
	remove := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
			t.Errorf("cleanup docker %v: %v\n%s", args, err, out)
		}
	}
	run("docker", "image", "import", "--change", `ENTRYPOINT ["/hostmesh-mcp"]`, rootfs, image)
	t.Cleanup(func() { remove("image", "rm", image) })
	t.Logf("shared test image: %s bytes", run("docker", "image", "inspect", "-f", "{{.Size}}", image))
	for _, network := range []string{left, right} {
		run("docker", "network", "create", "--internal", network)
		t.Cleanup(func() { remove("network", "rm", network) })
	}

	var ca testCA
	if mtls {
		ca = newTestCA(t)
	}
	start := func(name, network string, cfg config) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.StateDir = "/state"
		if mtls {
			until := time.Now().Add(time.Hour)
			if renewal {
				switch name {
				case a:
					until = time.Now().Add(3 * time.Minute)
					cfg.Peer.Renewal = &peer.RenewalConfig{URL: estURL}
				case c:
					cfg.Peer.Renewal = &peer.RenewalConfig{Relay: true}
				case b:
					cfg.Peer.Renewal = &peer.RenewalConfig{Gateway: "127.0.0.1:9443"}
				}
			}
			files := ca.issue(t, uuid.NewString(), until, name)
			for filename, source := range map[string]string{"ca.pem": files.CAFile, "node.pem": files.CertFile, "node.key": files.KeyFile} {
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg.Peer.CAFile, cfg.Peer.CertFile, cfg.Peer.KeyFile = "/state/ca.pem", "/state/node.pem", "/state/node.key"
		}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"create", "--pull=never", "--name", name, "--hostname", name, "--network", network,
			"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--log-driver=local", "--log-opt=max-size=1m", "--log-opt=max-file=2",
			"--mount", "type=bind,source=" + dir + ",destination=/state"}
		if name == b {
			args = append(args, "--env", "TZ=/tokyo.zone", "--mount", "type=bind,source="+osRelease+",destination=/etc/os-release,readonly")
		}
		run("docker", append(args, image, "serve", "-config", "/state/config.json")...)
		t.Cleanup(func() { remove("container", "rm", "-f", name) })
		run("docker", "start", name)
	}
	nodeID := func(name string) string {
		t.Helper()
		end := time.Now().Add(8 * time.Second)
		for time.Now().Before(end) {
			logs := run("docker", "logs", "--tail", "20", name)
			// A restarted node can have more than one startup line.
			for _, line := range slices.Backward(strings.Split(logs, "\n")) {
				if before, id, ok := strings.Cut(line, " node_id="); ok &&
					strings.HasPrefix(before, "hostmesh-mcp ") && uuid.Validate(id) == nil {
					return id
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("no startup ID for %s", name)
		return ""
	}
	probe := func(name string, args ...string) {
		t.Helper()
		cmd := append([]string{"exec", name, "/probe", "-test.run=^TestDockerNodeProbe$", "-test.v", "--"}, args...)
		t.Log(run("docker", cmd...))
	}

	start(c, left, config{Peer: peerConfig{Listen: ":7443", Relay: true}})
	run("docker", "network", "connect", right, c)
	initialC := nodeID(c)
	cfg := func(mcpListen string) config {
		return config{MCPListen: mcpListen, Peer: peerConfig{
			Listen: ":7443", Seeds: []string{c + ":7443"}, TrustedRelays: []string{initialC},
		}}
	}
	start(a, left, cfg("127.0.0.1:8787"))
	start(b, right, cfg(""))
	probe(b, "tcp-open", "127.0.0.1:7443")
	address := func(name string) string {
		t.Helper()
		ip := run("docker", "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
		if net.ParseIP(ip) == nil {
			t.Fatalf("invalid container IP %q for %s", ip, name)
		}
		return net.JoinHostPort(ip, "7443")
	}
	aEndpoint, bEndpoint := address(a), address(b)
	probe(c, "tcp-open", aEndpoint)
	probe(c, "tcp-open", bEndpoint)
	probe(a, "tcp-blocked", bEndpoint)
	probe(b, "tcp-blocked", aEndpoint)

	bID := nodeID(b)
	if renewal {
		// The EST authority only listens on the gateway's loopback device.
		// Its key mount is owned by a separate container. No peer receives it.
		issuerDir := filepath.Join(root, "issuer")
		if err := os.Mkdir(issuerDir, 0700); err != nil {
			t.Fatal(err)
		}
		caPath := fixtureAuthority(t, issuerDir, ca, "127.0.0.1:9443")
		originalKey := readFile(t, filepath.Join(root, a, "node.key"))
		originalCert := readFile(t, filepath.Join(root, a, "node.pem"))
		originID := nodeID(a)
		approvePeer(t, caPath, peerConfig{CertFile: filepath.Join(root, a, "node.pem"), KeyFile: filepath.Join(root, a, "node.key")}, originID, []string{a}, nil)
		issuer := prefix + "-issuer"
		run("docker", "run", "-d", "--pull=never", "--name", issuer, "--network", "container:"+b,
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--cap-drop=ALL", "--read-only",
			"--mount", "type=bind,source="+issuerDir+",destination=/issuer,readonly",
			"--mount", "type=bind,source="+filepath.Join(issuerDir, "state")+",destination=/issuer/state",
			image, "ca", "serve", "-config", "/issuer/ca.json")
		t.Cleanup(func() { remove("container", "rm", "-f", issuer) })
		probe(b, "tcp-open", "127.0.0.1:9443")
		probe(a, "tcp-blocked", "127.0.0.1:9443")
		probe(c, "tcp-blocked", "127.0.0.1:9443")
		host, _, _ := net.SplitHostPort(bEndpoint)
		probe(a, "tcp-blocked", net.JoinHostPort(host, "9443"))
		deadline := time.Now().Add(25 * time.Second)
		for bytes.Equal(readFile(t, filepath.Join(root, a, "node.pem")), originalCert) {
			if time.Now().After(deadline) {
				t.Fatalf("no renewal: %s", run("docker", "logs", "--tail", "12", a))
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !bytes.Equal(originalKey, readFile(t, filepath.Join(root, a, "node.key"))) {
			t.Fatal("changed applicant key")
		}
		probe(a, "renewed", originID)
		probe(a, "relay", bID, initialC, b, bEndpoint)
		return
	}
	probe(a, "relay", bID, initialC, b, bEndpoint)
	run("docker", "restart", c)
	probe(a, "relay", bID, initialC, b, bEndpoint)
	if got := nodeID(c); got != initialC {
		t.Fatalf("C changed identity after restart: got %s, want %s", got, initialC)
	}
}

// Invoked by the test binary inside a network namespace in TestDockerRelay.
func TestDockerNodeProbe(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) < i+3 {
		t.Skip("Docker probe entry point")
	}
	args := os.Args[i+1:]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	switch args[0] {
	case "tcp-open", "tcp-blocked":
		wantOpen := args[0] == "tcp-open"
		until := time.Now().Add(5 * time.Second)
		var err error
		for {
			var conn net.Conn
			conn, err = (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", args[1])
			if err == nil {
				conn.Close()
			}
			if err == nil || !wantOpen || time.Now().After(until) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if (err == nil) != (args[0] == "tcp-open") {
			t.Fatalf("%s %s: %v", args[0], args[1], err)
		}
		t.Logf("%s %s", args[0], args[1])
	case "renewed":
		data := readFile(t, "/state/node.pem")
		block, _ := pem.Decode(data)
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil || time.Until(leaf.NotAfter) < 40*time.Minute {
			t.Fatal("short-lived original certificate still on disk")
		}
		checkRenewedService(t, "http://127.0.0.1:8787/mcp", args[1], data)
	case "relay":
		if len(args) != 5 {
			t.Fatal("relay probe needs target ID, relay ID, target hostname and target address")
		}
		checkDockerRelay(ctx, t, args[1], args[2], args[3], args[4])
	default:
		t.Fatalf("unknown Docker probe %q", args[0])
	}
}

func checkDockerRelay(ctx context.Context, t *testing.T, target, relay, hostname, endpoint string) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "docker-lab", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:8787/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	params := &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{
		"target": map[string]string{"node_id": target}, "sections": []string{"system", "memory", "swap", "network", "processes", "storage", "clock"},
	}}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		reply, err := session.CallTool(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		if !reply.IsError {
			b, err := json.Marshal(reply.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var got status.Result
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if got.Node.ID != target || got.Node.Hostname != hostname || got.Route.Kind != "relay" ||
				got.Route.RelayID != relay || got.Sections.Memory == nil || got.Sections.Memory.TotalBytes == 0 {
				t.Fatalf("unexpected relay result: %+v", got)
			}
			host, _, err := net.SplitHostPort(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, iface := range got.Sections.Network {
				for _, addr := range iface.Addresses {
					if prefix, err := netip.ParsePrefix(addr); err == nil && prefix.Addr().String() == host {
						found = true
					}
				}
			}
			if !found || got.Sections.Swap == nil || got.Sections.Processes == nil || len(got.Sections.Processes.Items) == 0 {
				t.Fatalf("missing network, swap or processes from target: %+v", got)
			}
			if !hasRootMount(got.Sections.Storage) {
				t.Fatalf("missing target root mount: %+v", got.Errors)
			}
			checkKernelRelease(t, got.Sections.System)
			if system := got.Sections.System; system.OSID != "lab-linux" || system.OSName != "Lab Linux" ||
				system.OSVersionID != "2.4" || system.OSVersion != "2.4 (Test)" {
				t.Fatalf("B's distribution missing from relay result: %+v", system)
			}
			checkClock(t, got)
			if clock := got.Sections.Clock; clock.TimezoneAbbreviation != "JST" || clock.UTCOffsetSeconds != 9*60*60 {
				t.Fatalf("B's timezone missing from relay result: %+v", clock)
			}
			t.Logf("node %s reached through relay %s with target IP %s and %d visible processes",
				target, relay, host, got.Sections.Processes.Total)
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("relay never became available: %+v", reply.Content)
		}
	}
}
