package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMacPeer(t *testing.T) {
	ca := newTestCA(t)
	cert := func() peerConfig { return ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour)) }
	entry := cert()
	entry.Listen = "127.0.0.1:0"
	a := startServeProcess(t, config{Peer: entry, MCPListen: "127.0.0.1:0", AllowProcessKill: true})
	edge := cert()
	edge.Seeds = []string{a.peer}
	b := startServeProcess(t, config{Peer: edge})

	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var tr mcp.Transport = &mcp.StreamableClientTransport{Endpoint: a.mcp}
			if transport == "stdio" {
				tr = &mcp.CommandTransport{Command: appCommand(ctx, "stdio", "-state-dir", t.TempDir())}
			}
			client := mcp.NewClient(&mcp.Implementation{Name: "mac-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, tr, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tool := range tools.Tools {
				if tool.Name == "process_kill" || tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
					t.Fatalf("unexpected tool %+v", tool)
				}
			}
			if callServerConfig(t, session, false).AllowProcessKill {
				t.Fatal("effective config allows process_kill")
			}
			var proc status.Process
			if err := toolResult(ctx, session, "process_status", map[string]any{"pid": os.Getpid()}, &proc); err != nil {
				t.Fatal(err)
			}
			if proc.PID != int32(os.Getpid()) || proc.CPUPercent == nil || proc.ResidentBytes == nil || *proc.ResidentBytes == 0 || proc.Name == "" {
				t.Fatalf("incomplete process: %+v", proc)
			}
			args := map[string]any{"sections": []string{"system", "cpu", "processes", "network", "storage", "swap"}}
			if transport == "http" {
				checkUpgradePeer(t, session, b.id, strings.TrimSpace(versionFile))
				args["target"] = map[string]string{"node_id": b.id}
			}
			var got status.Result
			if err := toolResult(ctx, session, "host_status", args, &got); err != nil {
				t.Fatal(err)
			}
			s := got.Sections
			if got.Node.OS != "darwin" || transport == "http" && got.Node.ID != b.id || len(got.Errors) != 0 {
				t.Fatalf("unexpected node or section errors: %+v, %v", got.Node, got.Errors)
			}
			if s.System == nil || s.System.OSVersion == "" || s.CPU == nil || s.Swap == nil || s.Processes == nil || len(s.Processes.Items) == 0 {
				t.Fatalf("missing OS, CPU, swap or process information: %+v", s)
			}
			if !hasRootMount(s.Storage) || s.Storage.BlockDevices == nil || len(*s.Storage.BlockDevices) == 0 {
				t.Fatalf("missing disk inventory: %+v", s.Storage)
			}
			if !slices.ContainsFunc(s.Network, func(i status.Interface) bool { return len(i.Addresses) > 0 }) {
				t.Fatalf("missing interface addresses: %+v", s.Network)
			}
			for i := 1; i < len(s.Processes.Items); i++ {
				prev, cur := s.Processes.Items[i-1].CPUPercent, s.Processes.Items[i].CPUPercent
				if cur != nil && (prev == nil || *prev < *cur) {
					t.Fatal("process list is not ordered by CPU usage")
				}
			}
			// An unknown tool can close the SDK HTTP transport. Exercise it last.
			_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "process_kill", Arguments: map[string]any{"pid": 0}})
			if err == nil || !strings.Contains(err.Error(), "unknown tool") {
				t.Fatalf("expected unknown tool, got %v", err)
			}
		})
	}
}
