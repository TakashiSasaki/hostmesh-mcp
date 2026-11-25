package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/mcpserver"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedExtension(t *testing.T, value string) []pkix.Extension {
	t.Helper()
	der, err := asn1.MarshalWithParams(value, "utf8")
	if err != nil {
		t.Fatal(err)
	}
	return []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 54392, 3, 36202, 53280, 64414, 21169, 48647, 23055, 51379, 6491}, Value: der}}
}

func callServerConfig(t *testing.T, session *mcp.ClientSession, include bool) mcpserver.ServerConfig {
	t.Helper()
	args := map[string]any{}
	if include {
		args["include_certificate"] = true
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reply, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "server_config", Arguments: args})
	if err != nil || reply.IsError {
		t.Fatalf("server_config: %+v, %v", reply, err)
	}
	data, err := json.Marshal(reply.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var cfg mcpserver.ServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE KEY") || strings.Contains(string(data), "key_file") {
		t.Fatal("server_config exposed key material or key location")
	}
	return cfg
}

func checkServerConfig(t *testing.T, session *mcp.ClientSession, server readyProcess, leafPEM []byte, seeds []string) {
	t.Helper()
	for _, include := range []bool{true, false} {
		cfg := callServerConfig(t, session, include)
		if cfg.NodeID != server.id || cfg.MCPTransport != "http" || cfg.MCPListen == "" || "http://"+cfg.MCPListen+"/mcp" != server.mcp ||
			cfg.Peer.Listen != server.peer || !slices.Equal(cfg.Peer.Seeds, seeds) {
			t.Fatalf("unexpected runtime settings: %+v", cfg)
		}
		if leafPEM == nil {
			if cfg.Peer.Security != "plaintext" || cfg.Certificate != nil || cfg.Peer.SeedSource != "none" {
				t.Fatal("unexpected plaintext configuration")
			}
			continue
		}
		block, _ := pem.Decode(leafPEM)
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(leaf.Raw)
		cert := cfg.Certificate
		if cfg.Peer.Security != "mtls" || cfg.Peer.SeedSource != "certificate" || cert == nil ||
			cert.FingerprintSHA256 != hex.EncodeToString(sum[:]) || cert.Issuer != leaf.Issuer.String() ||
			!cert.NotAfter.Equal(leaf.NotAfter) || !cert.NotBefore.Equal(leaf.NotBefore) {
			t.Fatalf("wrong summary: %+v", cert)
		}
		if include && cert.PEM != string(leafPEM) || !include && cert.PEM != "" {
			t.Fatal("unexpected PEM output")
		}
	}
}

func TestCertificateSeedConfiguration(t *testing.T) {
	const seed = "a.example.internal:7443"
	ca := newTestCA(t)
	until := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name      string
		extension []pkix.Extension
		override  []string
		mcp       string
		want      string
	}{
		{"cert-only endpoint", seedExtension(t, seed), nil, "", "seeds: 1, source: certificate"},
		{"explicit endpoint", seedExtension(t, seed), []string{seed, "b.example.internal:7443"}, "", "seeds: 2, source: config"},
		{"explicit empty", seedExtension(t, seed), []string{}, "127.0.0.1:0", "seeds: 0, source: config"},
		{"empty disables only endpoint", seedExtension(t, seed), []string{}, "", "configure at least one"},
		{"invalid endpoint", seedExtension(t, "bad"), nil, "", "invalid certificate seed"},
		{"too many endpoints", seedExtension(t, strings.Repeat(seed+",", 16)+seed), nil, "", "more than 16"},
		{"legacy without endpoint", nil, nil, "", "configure at least one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ca.issueWithExtensions(t, uuid.NewString(), until, tc.extension)
			cfg.Seeds = tc.override
			data, err := json.Marshal(config{Peer: cfg, MCPListen: tc.mcp})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "cfg.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			out, err := appCommand(t.Context(), "config", "check", "-config", path).CombinedOutput()
			if (err == nil) != strings.HasPrefix(tc.want, "seeds:") || !strings.Contains(string(out), tc.want) {
				t.Fatalf("config check: %s, %v", out, err)
			}
		})
	}
}
