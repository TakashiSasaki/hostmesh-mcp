package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/product"
)

func TestDefaultStateDirectoryUpgrade(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "hostmesh-mcp", "state")
	previous := filepath.Join(root, product.LegacyName, "state")
	check := func(want string) {
		t.Helper()
		got, err := stateDirUnder(root)
		if err != nil || got != want {
			t.Fatalf("state path %q, error %v; want %q", got, err, want)
		}
	}
	check(current)
	for _, dir := range []string{current, previous} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(previous, "state.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	check(previous)
	if err := os.WriteFile(filepath.Join(previous, "state.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	check(previous)
	if err := os.WriteFile(filepath.Join(current, "state.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	check(current)
}
