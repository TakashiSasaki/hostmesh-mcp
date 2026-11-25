package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestConfigExample(t *testing.T) {
	for _, tc := range []struct {
		name       string
		flags      []string
		join, mtls bool
	}{
		{"listen", nil, false, false},
		{"join", []string{"-join"}, true, false},
		{"listen mtls", []string{"-mtls"}, false, true},
		{"join mtls", []string{"-join", "-mtls"}, true, true},
		{"mtls join", []string{"-mtls", "-join"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := appCommand(t.Context(), append([]string{"config", "example"}, tc.flags...)...).Output()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "peer.json")
			if err := os.WriteFile(path, output, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := readConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Peer
			if tc.join {
				if len(p.Seeds) != 1 || p.Listen != "" || len(p.Advertise) != 0 {
					t.Fatalf("invalid join template: %s", output)
				}
			} else if p.Listen == "" || !slices.Equal(p.Advertise, []string{p.Listen}) || len(p.Seeds) != 0 {
				t.Fatalf("invalid listener template: %s", output)
			}
			if tc.mtls != (p.Security == "mtls") {
				t.Fatalf("unexpected mode %q: %s", p.Security, output)
			}
		})
	}
}

func TestConfigExampleGuidance(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		success bool
		want    string
	}{
		{[]string{"serve"}, false, "host-status-mcp config example > peer.json"},
		{[]string{"config", "example", "-help"}, true, "[-join] [-mtls]"},
		{[]string{"config", "example", "-unknown"}, false, "unknown"},
		{[]string{"config", "example", "extra"}, false, "unexpected argument"},
	} {
		cmd := appCommand(t.Context(), tc.args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if (err == nil) != tc.success || len(out) != 0 || !strings.Contains(stderr.String(), tc.want) {
			t.Errorf("%v: status %v, stdout %q, stderr %q", tc.args, err, out, &stderr)
		}
	}
}
