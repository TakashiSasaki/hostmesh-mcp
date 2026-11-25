package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// All three processes share the host's hostname and IP. The observer stays
// running through the cutover and selects each rollout slot by node ID.
func TestSameHostUpgrade(t *testing.T) {
	current := strings.TrimSpace(versionFile)
	oldBinary := os.Getenv("HOST_STATUS_PREVIOUS_BINARY")
	previous := current
	if oldBinary != "" {
		out, err := exec.CommandContext(t.Context(), oldBinary, "version").Output()
		if err != nil {
			t.Fatal(err)
		}
		previous = strings.TrimSpace(string(out))
	}
	for _, mode := range []string{"plaintext", "mtls"} {
		t.Run(mode, func(t *testing.T) {
			var ca testCA
			if mode == "mtls" {
				ca = newTestCA(t)
			}
			instance := func(seeds ...string) config {
				var p peerConfig
				if mode == "mtls" {
					p = ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
				}
				p.Listen = "127.0.0.1:0"
				p.Seeds = seeds
				return config{MCPListen: "127.0.0.1:0", Peer: p}
			}
			observer := startServeProcess(t, instance())
			client := mcp.NewClient(&mcp.Implementation{Name: "upgrade-test", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: observer.mcp}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			old := startServeBinary(t, instance(observer.peer), oldBinary, previous)
			hostname := checkUpgradePeer(t, session, old.id, previous)
			next := startServeProcess(t, instance(observer.peer))
			if old.id == next.id || old.peer == next.peer || old.mcp == next.mcp {
				t.Fatal("old and new instances must have distinct node IDs and listeners")
			}
			if checkUpgradePeer(t, session, next.id, current) != hostname {
				t.Fatal("expected two peers on the same host")
			}
			checkUpgradePeer(t, session, old.id, previous)
			if err := old.process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-old.exited:
				if err != nil {
					t.Fatalf("old process failed to shut down cleanly: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old process did not terminate")
			}
			checkUpgradePeer(t, session, next.id, current)
		})
	}
}

func checkUpgradePeer(t *testing.T, session *mcp.ClientSession, id, version string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{
		"target": map[string]string{"node_id": id}, "sections": []string{"system"},
	}}
	for {
		reply, err := session.CallTool(ctx, request)
		if err != nil {
			t.Fatalf("query %s: %v", id, err)
		}
		if !reply.IsError {
			data, err := json.Marshal(reply.StructuredContent)
			var got status.Result
			if err != nil || json.Unmarshal(data, &got) != nil || got.Node.ID != id ||
				got.Node.Hostname == "" || got.Route.Kind != "direct" || got.Sections.System == nil || got.Sections.System.ServerVersion != version {
				t.Fatalf("expected %s version %s over a direct link, got %s", id, version, data)
			}
			return got.Node.Hostname
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out querying %s: %v", id, reply.Content)
		case <-time.After(25 * time.Millisecond):
		}
	}
}
