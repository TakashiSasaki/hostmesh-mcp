package mcpserver

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type PeerSettings struct {
	Security       string   `json:"security"`
	Listen         string   `json:"listen,omitempty"`
	Advertise      []string `json:"advertise,omitempty"`
	Seeds          []string `json:"seeds"`
	SeedSource     string   `json:"seed_source"`
	ShareAddresses bool     `json:"share_addresses"`
	Relay          bool     `json:"relay"`
	TrustedRelays  []string `json:"trusted_relays,omitempty"`
	DenyNodes      []string `json:"deny_nodes,omitempty"`
}

type ServerConfig struct {
	NodeID           string              `json:"node_id"`
	MCPTransport     string              `json:"mcp_transport"`
	MCPListen        string              `json:"mcp_listen,omitempty"`
	StateDir         string              `json:"state_dir"`
	Filesystems      []string            `json:"filesystems"`
	AllowProcessKill bool                `json:"allow_process_kill"`
	Peer             PeerSettings        `json:"peer"`
	Certificate      *certificateSummary `json:"certificate,omitempty"`
}

type certificateSummary struct {
	Issuer            string    `json:"issuer"`
	NotBefore         time.Time `json:"not_before"`
	NotAfter          time.Time `json:"not_after"`
	FingerprintSHA256 string    `json:"fingerprint_sha256"`
	PEM               string    `json:"pem,omitempty"`
}

func AddServerConfig(server *mcp.Server, cfg ServerConfig, leaf *x509.Certificate) {
	if cfg.Peer.Seeds == nil {
		cfg.Peer.Seeds = []string{}
	}
	if cfg.Filesystems == nil {
		cfg.Filesystems = []string{}
	}
	withPEM := cfg
	if leaf != nil {
		digest := sha256.Sum256(leaf.Raw)
		summary := certificateSummary{Issuer: leaf.Issuer.String(), NotBefore: leaf.NotBefore.UTC(),
			NotAfter: leaf.NotAfter.UTC(), FingerprintSHA256: hex.EncodeToString(digest[:])}
		cfg.Certificate = &summary
		full := summary
		full.PEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}))
		withPEM.Certificate = &full
	}
	type input struct {
		IncludeCertificate bool `json:"include_certificate,omitempty" jsonschema:"Include the loaded public leaf certificate in PEM format when mTLS is active."`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "server_config", Description: "Read the connected server's effective startup settings, peer seed source and local mTLS certificate summary.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, args input) (*mcp.CallToolResult, ServerConfig, error) {
			if args.IncludeCertificate {
				return nil, withPEM, nil
			}
			return nil, cfg, nil
		})
}
