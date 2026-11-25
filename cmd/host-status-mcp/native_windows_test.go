package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

func checkKernelRelease(t *testing.T, s *status.System) {
	t.Helper()
	v := windows.RtlGetVersion()
	want := fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	if s == nil || s.OSID != "windows" || s.KernelRelease != want {
		t.Fatalf("system=%+v, want Windows kernel %s", s, want)
	}
}

func TestWindowsNative(t *testing.T) {
	if os.Getenv("HOST_STATUS_STANDARD_USER") == "1" && windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("expected standard user")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	ca := newTestCA(t)
	entry := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	entry.Listen = "127.0.0.1:0"
	a := startServeProcess(t, config{Peer: entry, MCPListen: "127.0.0.1:0", AllowProcessKill: true})
	edge := ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour))
	edge.Seeds = []string{a.peer}
	b := startServeProcess(t, config{Peer: edge, Filesystems: []string{defaultFilesystemRoot()}})
	client := mcp.NewClient(&mcp.Implementation{Name: "windows-test", Version: "1"}, nil)
	s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: a.mcp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if callServerConfig(t, s, false).AllowProcessKill {
		t.Fatal("expected read-only Windows tools")
	}
	var p status.Process
	if err := toolResult(ctx, s, "process_status", map[string]any{"pid": os.Getpid()}, &p); err != nil {
		t.Fatal(err)
	}
	if p.PID != int32(os.Getpid()) || !strings.HasSuffix(strings.ToLower(p.Name), ".exe") || p.CPUPercent == nil || p.ResidentBytes == nil || *p.ResidentBytes == 0 {
		t.Fatalf("missing Windows process details: %+v", p)
	}
	checkUpgradePeer(t, s, b.id, strings.TrimSpace(versionFile))
	args := map[string]any{"target": map[string]string{"node_id": b.id}, "sections": status.SupportedSections()}
	var r status.Result
	if err := toolResult(ctx, s, "host_status", args, &r); err != nil {
		t.Fatal(err)
	}
	m := r.Sections
	if r.Node.ID != b.id || r.Node.OS != "windows" || m.CPU == nil || m.Memory == nil || m.Memory.TotalBytes == 0 || m.Processes == nil || len(m.Processes.Items) == 0 || len(m.Network) == 0 || len(m.Filesystems) == 0 {
		t.Fatalf("missing native metrics: %+v, %v", m, r.Errors)
	}
	checkKernelRelease(t, m.System)
	checkClock(t, r)
	if !hasRootMount(m.Storage) || m.Storage.BlockDevices == nil || !slices.ContainsFunc(*m.Storage.BlockDevices, func(d status.BlockDevice) bool { return d.Kind == "volume" && d.SizeBytes != nil && *d.SizeBytes > 0 }) {
		t.Fatalf("expected Windows volume inventory: %+v, %v", m.Storage, r.Errors)
	}
	swap := m.Swap
	if swap == nil || swap.TotalBytes != swap.FreeBytes+swap.UsedBytes || swap.Enabled != (len(swap.PageFiles) > 0) {
		t.Fatalf("invalid pagefile usage: %+v, %v", swap, r.Errors)
	}
	for _, f := range swap.PageFiles {
		if f.Name == "" || f.TotalBytes < f.UsedBytes {
			t.Fatalf("invalid pagefile: %+v", f)
		}
	}
}
