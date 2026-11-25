package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func appCommand(ctx context.Context, args ...string) *exec.Cmd {
	if path := os.Getenv("HOST_STATUS_TEST_BINARY"); path != "" {
		return exec.CommandContext(ctx, path, args...)
	}
	return exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestEntrypoint$", "--"}, args...)...)
}

type readyProcess struct{ id, peer, mcp string }

func startServeProcess(t *testing.T, c config) readyProcess {
	t.Helper()
	dir := t.TempDir()
	c.StateDir = filepath.Join(dir, "state")
	path := filepath.Join(dir, "config.json")
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cmd := appCommand(ctx, "serve", "-config", path)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	ready := make(chan string, 3)
	go func() {
		defer close(ready)
		scan := bufio.NewScanner(stderr)
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "host-status-mcp ") || strings.HasPrefix(line, "peer listening on ") || strings.HasPrefix(line, "MCP listening on ") {
				ready <- line
			}
		}
	}()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	var r readyProcess
	for r.id == "" || c.Peer.Listen != "" && r.peer == "" || c.MCPListen != "" && r.mcp == "" {
		select {
		case line, ok := <-ready:
			if !ok {
				t.Fatalf("serve exited before readiness: %+v", r)
			}
			switch {
			case strings.HasPrefix(line, "host-status-mcp "):
				want := "host-status-mcp " + strings.TrimSpace(versionFile) + " node_id="
				id, ok := strings.CutPrefix(line, want)
				if !ok {
					t.Fatalf("wrong version in startup message: %s", line)
				}
				r.id = id
			case strings.HasPrefix(line, "peer listening on "):
				r.peer = strings.TrimPrefix(line, "peer listening on ")
			case strings.HasPrefix(line, "MCP listening on "):
				r.mcp = strings.TrimPrefix(line, "MCP listening on ")
			}
		case <-deadline.C:
			t.Fatalf("serve readiness timeout: %+v", r)
		}
	}
	return r
}

func TestServeRelayProcesses(t *testing.T) {
	for _, mode := range []string{"plaintext", "mtls"} {
		t.Run(mode, func(t *testing.T) {
			mk := func() peerConfig { return peerConfig{Security: "plaintext"} }
			if mode == "mtls" {
				ca := newTestCA(t)
				mk = func() peerConfig { return ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour)) }
			}
			testServeRelay(t, mk)
		})
	}
}

func testServeRelay(t *testing.T, mk func() peerConfig) {
	hub := mk()
	hub.Listen, hub.Relay = "127.0.0.1:0", true
	c := startServeProcess(t, config{Peer: hub})
	edge := func() peerConfig {
		cfg := mk()
		cfg.Seeds = []string{c.peer}
		cfg.TrustedRelays = []string{c.id}
		return cfg
	}
	missingA := filepath.Join(t.TempDir(), "missing-a")
	missingB := filepath.Join(t.TempDir(), "missing-b")
	b := startServeProcess(t, config{Peer: edge(), Filesystems: []string{missingB}})
	a := startServeProcess(t, config{MCPListen: "127.0.0.1:0", Peer: edge(), Filesystems: []string{missingA}})

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.mcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://unrelated.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status %d", resp.StatusCode)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: a.mcp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().ServerInfo.Version; got != strings.TrimSpace(versionFile) {
		t.Fatalf("server version %q", got)
	}
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	for {
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{
			"target": map[string]string{"node_id": b.id}, "sections": []string{"memory"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.IsError {
			var got status.Result
			content, err := json.Marshal(reply.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(content, &got); err != nil {
				t.Fatal(err)
			}
			if got.Node.ID != b.id || got.Route.Kind != "relay" || got.Route.RelayID != c.id ||
				got.Sections.Memory == nil || got.Sections.Memory.TotalBytes == 0 {
				t.Fatalf("unexpected native process result: %+v", got)
			}
			break
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatalf("native process relay did not become available: %v", reply.Content)
		}
	}

	reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_list"})
	if err != nil || reply.IsError {
		t.Fatalf("host_list = %+v, %v", reply, err)
	}
	var listed struct {
		Hosts []status.Host `json:"hosts"`
	}
	data, err := json.Marshal(reply.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &listed); err != nil {
		t.Fatal(err)
	}
	localFound, remoteFound := false, false
	for _, host := range listed.Hosts {
		if host.IsLocal != (host.ID == a.id) {
			t.Fatalf("wrong local marker for %s", host.ID)
		}
		localFound = localFound || host.ID == a.id
		remoteFound = remoteFound || host.ID == b.id
	}
	if !localFound || !remoteFound {
		t.Fatalf("host_list is missing A or B: %+v", listed.Hosts)
	}
	for _, failureCase := range []struct{ nodeID, path string }{{"", missingA}, {b.id, missingB}} {
		args := map[string]any{"sections": []string{"filesystems"}}
		if failureCase.nodeID != "" {
			args["target"] = map[string]string{"node_id": failureCase.nodeID}
		}
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: args})
		if err != nil || !reply.IsError || len(reply.Content) != 1 {
			t.Fatalf("host_status failure = %+v, %v", reply, err)
		}
		content, ok := reply.Content[0].(*mcp.TextContent)
		var failure status.Failure
		if !ok || json.Unmarshal([]byte(content.Text), &failure) != nil ||
			failure.Code != "COLLECTION_FAILED" || failure.SectionErrors["filesystems:"+failureCase.path] == "" {
			t.Fatalf("missing filesystem diagnostics for %q: %+v", failureCase.nodeID, reply.Content)
		}
	}
}

func TestPeerSecurityConfiguration(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.pem")
	for _, tc := range []struct {
		name string
		cfg  peerConfig
		want string
	}{
		{"default plaintext", peerConfig{}, "plaintext"},
		{"default mtls", peerConfig{CAFile: missing, CertFile: missing, KeyFile: missing}, "mtls"},
		{"explicit plaintext", peerConfig{Security: "plaintext"}, "plaintext"},
		{"explicit mtls", peerConfig{Security: "mtls", CAFile: missing, CertFile: missing, KeyFile: missing}, "mtls"},
		{"explicit plaintext with credentials", peerConfig{Security: "plaintext", CAFile: missing, CertFile: missing, KeyFile: missing}, ""},
		{"explicit mtls without credentials", peerConfig{Security: "mtls"}, ""},
		{"unknown mode", peerConfig{Security: "auto"}, ""},
		{"CA only", peerConfig{CAFile: missing}, ""},
		{"cert only", peerConfig{CertFile: missing}, ""},
		{"key only", peerConfig{KeyFile: missing}, ""},
		{"missing CA", peerConfig{CertFile: missing, KeyFile: missing}, ""},
		{"missing cert", peerConfig{CAFile: missing, KeyFile: missing}, ""},
		{"missing key", peerConfig{CAFile: missing, CertFile: missing}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Listen = "127.0.0.1:0"
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(config{Peer: tc.cfg, StateDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := readConfig(path)
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted invalid configuration")
				}
				return
			}
			if err != nil || c.Peer.Security != tc.want {
				t.Fatalf("mode = %q, err = %v", c.Peer.Security, err)
			}
			if tc.want == "mtls" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := serve(ctx, "test", c); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("expected missing certificate file error, got %v", err)
				}
			}
		})
	}
}
