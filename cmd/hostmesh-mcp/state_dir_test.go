package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/product"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/state"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
)

func TestDefaultStateDirectoryUpgrade(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "hostmesh-mcp", "state")
	previous := filepath.Join(root, product.LegacyName, "state")
	checkDefault := func() {
		t.Helper()
		got, err := stateDirUnder(root)
		if err != nil || got != current {
			t.Fatalf("state path %q, error %v; want %q", got, err, current)
		}
	}
	checkMigrationRequired := func() {
		t.Helper()
		got, err := stateDirUnder(root)
		if got != "" || err == nil || !strings.Contains(err.Error(), "move") {
			t.Fatalf("expected migration instructions, got %q, %v", got, err)
		}
	}
	checkDefault()
	for _, dir := range []string{current, previous} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	checkDefault()
	if err := os.WriteFile(filepath.Join(previous, "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	checkMigrationRequired()
	local := status.Node{Hostname: "test-peer"}
	store, err := state.OpenLocal(previous, local)
	if err != nil {
		t.Fatal(err)
	}
	id := store.Self().ID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(previous, "state.lock")); err != nil {
		t.Fatal(err)
	}
	checkMigrationRequired()
	if err := os.Rename(filepath.Join(previous, "state.json"), filepath.Join(current, "state.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	checkDefault()
	reopened, err := state.OpenLocal(current, local)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Self().ID; got != id {
		t.Fatalf("node ID after migration %q; want %q", got, id)
	}
	if _, err := os.Stat(filepath.Join(current, "state.lock")); err != nil {
		t.Fatal(err)
	}
}
