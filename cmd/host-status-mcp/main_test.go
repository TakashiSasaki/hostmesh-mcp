package main

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
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
		cmd := appCommand(ctx, "stdio", "-state-dir", dir)
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if version := session.InitializeResult().ServerInfo.Version; version != strings.TrimSpace(versionFile) {
			t.Fatalf("server advertised version %q", version)
		}
		tools, err := session.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 4 {
			t.Fatalf("tool list = %v, err = %v", tools, err)
		}
		for _, tool := range tools.Tools {
			if tool.Name == "process_kill" {
				a := tool.Annotations
				if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
					t.Fatalf("wrong write annotations: %+v", a)
				}
			} else if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Fatalf("not read only: %s", tool.Name)
			}
		}
		versionReply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "server_version", Arguments: map[string]any{}})
		if err != nil || versionReply.IsError {
			t.Fatalf("server_version = %v, err = %v", versionReply, err)
		}
		versionResult, ok := versionReply.StructuredContent.(map[string]any)
		if !ok || versionResult["version"] != strings.TrimSpace(versionFile) {
			t.Fatalf("server_version returned %v", versionReply.StructuredContent)
		}
		before := time.Now()
		reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status", Arguments: map[string]any{"sections": []string{"system", "cpu", "memory", "filesystems", "storage", "clock"}}})
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
		if r.Sections.System == nil || r.Sections.System.ServerVersion != strings.TrimSpace(versionFile) || r.Sections.CPU == nil || r.Sections.Memory == nil || len(r.Sections.Filesystems) != 1 {
			t.Fatalf("missing metric: %+v", r)
		}
		if !hasRootMount(r.Sections.Storage) {
			t.Fatalf("missing local root mount: %+v", r.Errors)
		}
		checkClock(t, r)
		if r.ObservedAt.Before(before) || r.ObservedAt.After(time.Now()) {
			t.Fatalf("clock reading %s is outside the call interval", r.ObservedAt)
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

func hasRootMount(storage *status.Storage) bool {
	return storage != nil && storage.Mounts != nil && slices.ContainsFunc(*storage.Mounts, func(m status.Mount) bool {
		return m.MountPoint == "/" && m.FilesystemType != ""
	})
}

func checkClock(t *testing.T, r status.Result) {
	t.Helper()
	clock := r.Sections.Clock
	if clock == nil {
		t.Fatalf("missing clock section: %+v", r.Errors)
	}
	_, utcOffset := clock.UTCTime.Zone()
	_, localOffset := clock.LocalTime.Zone()
	if !clock.UTCTime.Equal(r.ObservedAt) || !clock.LocalTime.Equal(clock.UTCTime) ||
		utcOffset != 0 || localOffset != clock.UTCOffsetSeconds || clock.TimezoneAbbreviation == "" {
		t.Fatalf("inconsistent clock reading: %+v, observed_at %s", clock, r.ObservedAt)
	}
}
