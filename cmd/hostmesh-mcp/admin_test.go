package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func adminCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args any, wantError bool) map[string]any {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError != wantError {
		t.Fatalf("%s: %+v", name, res.Content)
	}
	if wantError {
		return nil
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("missing structured %s result", name)
	}
	return out
}

func TestAdministration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	dir := t.TempDir()
	ca := filepath.Join(dir, "test-ca", "online", "ca.json")
	path := filepath.Join(dir, "peer.json")
	client := mcp.NewClient(&mcp.Implementation{Name: "operator", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: appCommand(ctx, "mcp", "-peer-config", path, "-ca-config", ca)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = tool
	}
	if names["ca_inspect_csr"] == nil || !names["ca_inspect_csr"].Annotations.ReadOnlyHint || names["ca_revoke"] == nil || !*names["ca_revoke"].Annotations.DestructiveHint {
		t.Fatal("missing tool annotations")
	}
	for _, name := range []string{"update_check", "update_apply", "supervisor_start", "process_kill"} {
		if (names[name] != nil) != (runtime.GOOS == "linux") {
			t.Fatalf("unexpected availability: %s", name)
		}
	}
	call := func(name string, in any) map[string]any { return adminCall(t, ctx, session, name, in, false) }
	reject := func(name string, in any) { adminCall(t, ctx, session, name, in, true) }
	call("ca_bootstrap", map[string]any{"server_name": "ca.example.test"})
	call("config_check", map[string]any{"kind": "ca"})
	generated := call("peer_csr", map[string]any{"directory": filepath.Join(dir, "key")})
	csr := map[string]any{"csr_pem": generated["csr_pem"], "lookup_registry": true}
	if call("ca_inspect_csr", csr)["registry_status"] != "unregistered" {
		t.Fatal("unexpected initial registry")
	}
	enroll := map[string]any{"csr_pem": generated["csr_pem"], "expected_csr_sha256": strings.Repeat("a", 64), "profile": map[string]any{"node_id": "7043f65e-b1f6-489d-a0d2-5d402035735d", "dns_names": []string{"sensor.example.test"}}}
	reject("ca_enroll", enroll)
	if call("ca_inspect_csr", csr)["registry_status"] != "unregistered" {
		t.Fatal("mismatch changed registry")
	}
	enroll["expected_csr_sha256"] = generated["csr_sha256"]
	issued := call("ca_enroll", enroll)
	creds := call("peer_install_certificate", map[string]any{"directory": filepath.Join(dir, "public"), "key_file": generated["key_file"], "certificate_pem": issued["certificate_pem"], "ca_pem": issued["ca_pem"]})
	config := map[string]any{"mcp_listen": "127.0.0.1:0", "state_dir": filepath.Join(dir, "state"), "peer": map[string]any{"listen": "127.0.0.1:0", "ca_file": creds["ca_file"], "cert_file": creds["cert_file"], "key_file": creds["key_file"]}}
	save := map[string]any{"kind": "peer", "config": config}
	first := call("config_save", save)
	reject("config_save", save)
	args := map[string]any{"kind": "peer"}
	if read := call("config_read", args); read["sha256"] != first["sha256"] {
		t.Fatal("revision mismatch")
	}
	save["expected_sha256"] = first["sha256"]
	config["filesystems"] = []string{defaultFilesystemRoot()}
	second := call("config_save", save)
	reject("config_save", save)
	if read := call("config_read", args); read["sha256"] != second["sha256"] {
		t.Fatal("stale edit replaced file")
	}
	call("config_check", args)
	no := map[string]any{}
	started := call("peer_start", no)["details"].(map[string]any)
	if started["node_id"] != issued["node_id"] {
		t.Fatal("running identity differs from issued identity")
	}
	endpoint := started["mcp_url"].(string)
	web, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	public, err := web.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range public.Tools {
		if names[tool.Name] != nil && tool.Name != "process_status" {
			t.Fatalf("admin tool %s exposed over HTTP", tool.Name)
		}
	}
	web.Close()
	reject("peer_start", no)
	call("service_stop", map[string]any{"service": "peer"})
	call("ca_revoke", map[string]any{"certificate_pem": issued["certificate_pem"]})
	if call("ca_inspect_csr", csr)["registry_status"] != "revoked" {
		t.Fatal("revocation not visible")
	}
	reject("ca_enroll", enroll)
	caRead := call("config_read", map[string]any{"kind": "ca"})
	caData := caRead["config"].(map[string]any)
	caData["listen"] = "127.0.0.1:0"
	call("config_save", map[string]any{"kind": "ca", "config": caData, "expected_sha256": caRead["sha256"]})
	addr := call("ca_start", no)["details"].(map[string]any)["listen"].(string)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	// The subprocess must release its session-owned listener on stdin EOF.
	deadline := time.Now().Add(4 * time.Second)
	for {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAdminShell(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peer.json")
	cmd := appCommand(t.Context(), "admin", "-peer-config", path, "call", "config_save", "-")
	cmd.Stdin = strings.NewReader(`{"kind":"peer","config":{"mcp_listen":"127.0.0.1:0"}}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	var saved map[string]any
	if err = json.Unmarshal(out, &saved); err != nil || saved["path"] != path {
		t.Fatalf("%s %v", out, err)
	}
	if err = appCommand(t.Context(), "config", "check", "-config", path).Run(); err != nil {
		t.Fatal(err)
	}
	info, err := appCommand(t.Context(), "admin", "describe", "ca_enroll").Output()
	if err != nil || !strings.Contains(string(info), "\"expected_csr_sha256\"") {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
