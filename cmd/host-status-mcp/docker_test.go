//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDockerRelay(t *testing.T) {
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

	build("build", "-o", filepath.Join(imageDir, "host-status-mcp"), ".")
	build("test", "-c", "-o", filepath.Join(imageDir, "probe"), ".")
	rootfs := filepath.Join(root, "rootfs.tar")
	run("tar", "-C", imageDir, "-cf", rootfs, "host-status-mcp", "probe")

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
	run("docker", "image", "import", "--change", `ENTRYPOINT ["/host-status-mcp"]`, rootfs, image)
	t.Cleanup(func() { remove("image", "rm", image) })
	t.Logf("shared test image: %s bytes", run("docker", "image", "inspect", "-f", "{{.Size}}", image))
	for _, network := range []string{left, right} {
		run("docker", "network", "create", "--internal", network)
		t.Cleanup(func() { remove("network", "rm", network) })
	}

	start := func(name, network string, cfg config) {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		cfg.StateDir = "/state"
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "config.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		run("docker", "create", "--pull=never", "--name", name, "--hostname", name, "--network", network,
			"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
			"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
			"--log-driver=local", "--log-opt=max-size=1m", "--log-opt=max-file=2",
			"--mount", "type=bind,source="+dir+",destination=/state",
			image, "serve", "-config", "/state/config.json")
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
					strings.HasPrefix(before, "host-status-mcp ") && uuid.Validate(id) == nil {
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

	start(c, left, config{Peer: peerConfig{Security: "plaintext", Listen: ":7443", Relay: true}})
	run("docker", "network", "connect", right, c)
	initialC := nodeID(c)
	cfg := func(mcpListen string) config {
		return config{MCPListen: mcpListen, Peer: peerConfig{
			Security: "plaintext", Listen: ":7443", Seeds: []string{c + ":7443"}, TrustedRelays: []string{initialC},
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
		"target": map[string]string{"node_id": target}, "sections": []string{"memory", "swap", "network", "processes", "storage"},
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
