package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A command writing its release report to the transport would corrupt the next response.
func TestAdminUpdate(t *testing.T) {
	f := testReleaseFeed(t)
	f.publish(t, "0.0.34", []byte("fixture"))
	f.config.Binary = filepath.Join(t.TempDir(), "peer")
	if err := os.WriteFile(f.config.Binary, []byte("#!/bin/sh\necho 0.0.34\n"), 0700); err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "update.json")
	if err = os.WriteFile(path, settings, 0600); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	client := mcp.NewClient(&mcp.Implementation{Name: "update-test", Version: "test"}, nil)
	s, err := client.Connect(ctx, &mcp.CommandTransport{Command: appCommand(ctx, "mcp", "-update-config", path)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, op := range []string{"update_check", "update_apply"} {
		r := adminCall(t, ctx, s, op, map[string]any{}, false)
		if r["report"] != "Installed 0.0.34, published 0.0.34\n" {
			t.Fatal(r)
		}
	}
	adminCall(t, ctx, s, "admin_info", map[string]any{}, false)
}
