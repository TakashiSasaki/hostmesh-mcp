package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestEntrypoint(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	if err := run(context.Background(), os.Args[i+1:]); err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}

func TestStdio(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var previous string
	for range 2 {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEntrypoint$", "--", "stdio", "-state-dir", dir)
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		tools, err := session.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 2 {
			t.Fatalf("tool list = %v, err = %v", tools, err)
		}
		for _, tool := range tools.Tools {
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("not read only: %s", tool.Name)
			}
		}
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{"sections": []string{"system", "cpu", "memory", "filesystems"}}})
		if err != nil || reply.IsError {
			t.Fatalf("status = %v, err = %v", reply, err)
		}
		b, err := json.Marshal(reply.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var r status.Result
		if err := json.Unmarshal(b, &r); err != nil {
			t.Fatal(err)
		}
		if r.Node.ID == "" || r.Node.OS != "linux" || r.ObservedAt.IsZero() || r.Route.Kind != "local" {
			t.Fatalf("invalid local result: %+v", r)
		}
		if r.Sections.System == nil || r.Sections.CPU == nil || r.Sections.Memory == nil || len(r.Sections.Filesystems) != 1 {
			t.Fatalf("missing metric: %+v", r)
		}
		if previous != "" && r.Node.ID != previous {
			t.Fatalf("ID changed from %s to %s", previous, r.Node.ID)
		}
		previous = r.Node.ID
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
