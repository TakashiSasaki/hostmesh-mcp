package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
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
	c := startServeProcess(t, config{Peer: peerConfig{Security: "plaintext", Listen: "127.0.0.1:0", Relay: true}})
	cfg := peerConfig{Security: "plaintext", Seeds: []string{c.peer}, TrustedRelays: []string{c.id}}
	b := startServeProcess(t, config{Peer: cfg})
	a := startServeProcess(t, config{MCPListen: "127.0.0.1:0", Peer: cfg})

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
			return
		}
		select {
		case <-poll.C:
		case <-ctx.Done():
			t.Fatalf("native process relay did not become available: %v", reply.Content)
		}
	}
}
