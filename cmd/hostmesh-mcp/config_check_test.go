package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestConfigCheck(t *testing.T) {
	// An occupied socket and a nonexistent state dir test check-only behavior.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	ca := newTestCA(t)
	id := uuid.NewString()
	valid := ca.issue(t, id, time.Now().Add(time.Hour))
	wrongKey := valid
	wrongKey.KeyFile = ca.issue(t, uuid.NewString(), time.Now().Add(time.Hour)).KeyFile
	missing := valid
	missing.CAFile = filepath.Join(dir, "missing.pem")
	denied := valid
	denied.DenyNodes = []string{id}
	configJSON := func(p peerConfig) string {
		data, err := json.Marshal(config{MCPListen: listener.Addr().String(), StateDir: stateDir, Peer: p})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tc := range []struct {
		name, data, mode, problem string
	}{
		{"plaintext", configJSON(peerConfig{Seeds: []string{"offline.invalid:7443"}}), "plaintext", ""},
		{"mtls", configJSON(valid), "mtls", ""},
		{"missing CA file", configJSON(missing), "", "missing.pem"},
		{"expired cert", configJSON(ca.issue(t, id, time.Now().Add(-time.Minute))), "", "expired"},
		{"wrong private key", configJSON(wrongKey), "", "private key"},
		{"denied signer", configJSON(denied), "", "cannot deny"},
		{"partial credentials", `{"peer":{"ca_file":"ca.pem"}}`, "", "requires"},
		{"unknown field", `{"peer":{"seed":[]}}`, "", "unknown field"},
		{"invalid JSON", `{`, "", "decode configuration"},
		{"not an object", `null`, "", "one JSON object"},
		{"extra object", `{} {}`, "", "one JSON object"},
		{"input over size limit", "{}" + strings.Repeat(" ", 1<<20), "", "exceeds"},
		{"bad listen", `{"peer":{"listen":"no-port"}}`, "", "peer.listen"},
		{"bad MCP port", `{"mcp_listen":"127.0.0.1:65536"}`, "", "mcp_listen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := appCommand(t.Context(), "config", "check", "-config", path)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if tc.problem == "" {
				if err != nil || !strings.HasPrefix(string(out), "Configuration OK (peer mode: "+tc.mode+", seeds: ") || stderr.Len() != 0 {
					t.Fatalf("check: %v, stdout=%q stderr=%q", err, out, stderr.String())
				}
			} else if cmd.ProcessState.ExitCode() != 1 || len(out) != 0 || !strings.Contains(stderr.String(), tc.problem) {
				t.Fatalf("expected error %q: %v, stdout=%q stderr=%q", tc.problem, err, out, stderr.String())
			}
		})
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check touched state dir: %v", err)
	}
}
