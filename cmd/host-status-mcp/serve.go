package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/mcpserver"
	"github.com/TakashiSasaki/host-status-mcp/internal/peer"
	"github.com/TakashiSasaki/host-status-mcp/internal/state"
	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
)

type config struct {
	AllowProcessKill bool       `json:"allow_process_kill,omitempty"`
	MCPListen        string     `json:"mcp_listen"`
	StateDir         string     `json:"state_dir"`
	Filesystems      []string   `json:"filesystems"`
	Peer             peerConfig `json:"peer"`
}

type peerConfig struct {
	Renewal        *peer.RenewalConfig `json:"renewal,omitempty"`
	CAFile         string              `json:"ca_file,omitempty"`
	CertFile       string              `json:"cert_file,omitempty"`
	KeyFile        string              `json:"key_file,omitempty"`
	Security       string              `json:"security,omitempty"`
	Listen         string              `json:"listen,omitempty"`
	Advertise      []string            `json:"advertise,omitempty"`
	Seeds          seedList            `json:"seeds,omitzero"`
	seedSource     string
	ShareAddresses *bool    `json:"share_addresses,omitempty"`
	Relay          bool     `json:"relay,omitempty"`
	TrustedRelays  []string `json:"trusted_relays,omitempty"`
	DenyNodes      []string `json:"deny_nodes,omitempty"`
}

// Keep a present empty list (including a legacy JSON null) distinct from
// an absent setting, so an explicit override never inherits certificate seeds.
type seedList []string

func (s *seedList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = seedList{}
		return nil
	}
	return json.Unmarshal(data, (*[]string)(s))
}

func readConfig(path string) (config, error) {
	var c = config{MCPListen: "127.0.0.1:8787", Filesystems: []string{"/"}}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	const maxConfigSize = 1 << 20
	reader := &io.LimitedReader{R: f, N: maxConfigSize + 1}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	object := &c
	if err := decoder.Decode(&object); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF || object == nil {
		return c, fmt.Errorf("configuration must contain one JSON object")
	}
	if reader.N == 0 {
		return c, fmt.Errorf("configuration exceeds %d bytes", maxConfigSize)
	}
	if c.MCPListen != "" {
		host, err := listenHost(c.MCPListen)
		if err != nil {
			return c, fmt.Errorf("mcp_listen: %w", err)
		}
		if ip, err := netip.ParseAddr(host); err != nil || !ip.IsLoopback() {
			return c, fmt.Errorf("mcp_listen must be a loopback IP and port")
		}
	}
	if c.Peer.Listen != "" {
		if _, err := listenHost(c.Peer.Listen); err != nil {
			return c, fmt.Errorf("peer.listen: %w", err)
		}
	}
	mode := "plaintext"
	if c.Peer.CAFile != "" || c.Peer.CertFile != "" || c.Peer.KeyFile != "" {
		if c.Peer.CAFile == "" || c.Peer.CertFile == "" || c.Peer.KeyFile == "" {
			return c, fmt.Errorf("mTLS requires peer.ca_file, peer.cert_file and peer.key_file together")
		}
		mode = "mtls"
	}
	if c.Peer.Security != "" && c.Peer.Security != mode {
		return c, fmt.Errorf("peer.security %q conflicts with mode %q selected by certificate settings", c.Peer.Security, mode)
	}
	c.Peer.Security = mode
	if c.Peer.Renewal != nil {
		if mode != "mtls" {
			return c, fmt.Errorf("peer.renewal requires mTLS")
		}
		if err := c.Peer.Renewal.Validate(); err != nil {
			return c, err
		}
	}
	if c.Peer.Listen == "" && len(c.Peer.Advertise) != 0 {
		return c, fmt.Errorf("peer.advertise requires peer.listen")
	}
	if len(c.Peer.Advertise) > 16 || len(c.Peer.Seeds) > peer.MaxSeeds {
		return c, fmt.Errorf("at most 16 advertised endpoints or seeds are supported")
	}
	for _, e := range slices.Concat(c.Peer.Advertise, []string(c.Peer.Seeds)) {
		if !peer.ValidEndpoint(e) {
			return c, fmt.Errorf("invalid peer endpoint %q", e)
		}
	}
	for _, id := range slices.Concat(c.Peer.DenyNodes, c.Peer.TrustedRelays) {
		u, err := uuid.Parse(id)
		if err != nil || u.Version() != 4 || id != u.String() {
			return c, fmt.Errorf("invalid node ID %q", id)
		}
	}
	for _, path := range c.Filesystems {
		if !filepath.IsAbs(path) {
			return c, fmt.Errorf("filesystem path must be absolute: %q", path)
		}
	}
	return c, nil
}

// Validate the socket address and TCP service without resolving the host or binding.
func listenHost(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	_, err = net.LookupPort("tcp", port)
	return host, err
}

func (p peerConfig) loadTLS() (*peer.PeerTLS, error) {
	if p.Security != "mtls" {
		return nil, nil
	}
	security, err := peer.LoadPeerTLS(p.CAFile, p.CertFile, p.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("peer mTLS: %w", err)
	}
	if slices.Contains(p.DenyNodes, security.NodeID()) {
		return nil, fmt.Errorf("cannot deny the local node")
	}
	return security, nil
}

func (c *config) preparePeering() (*peer.PeerTLS, error) {
	security, err := c.Peer.loadTLS()
	if err != nil {
		return nil, err
	}
	if c.Peer.Renewal != nil && c.Peer.Renewal.URL != "" {
		if security == nil || !slices.Contains(security.LocalCertificate().ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
			return nil, errors.New("automatic renewal requires a peer certificate with clientAuth")
		}
		// Replacing the public bundle must never remove a key or trust anchor,
		// including when the operator uses hard links or symlinks.
		leafFile, err := os.Stat(c.Peer.CertFile)
		if err != nil {
			return nil, err
		}
		for _, path := range []string{c.Peer.KeyFile, c.Peer.CAFile} {
			other, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if os.SameFile(leafFile, other) {
				return nil, errors.New("automatic renewal requires cert_file separate from key_file and ca_file")
			}
		}
	}
	c.Peer.seedSource = "none"
	if c.Peer.Seeds != nil {
		c.Peer.seedSource = "config"
	} else if security != nil {
		seeds, err := security.BootstrapSeeds()
		if err != nil {
			return nil, err
		}
		if seeds != nil {
			c.Peer.Seeds, c.Peer.seedSource = seeds, "certificate"
		}
	}
	if c.MCPListen == "" && c.Peer.Listen == "" && len(c.Peer.Seeds) == 0 {
		return nil, fmt.Errorf("configure at least one MCP or peer endpoint")
	}
	return security, nil
}

var errCertificateRenewed = errors.New("certificate renewed")

func serve(ctx context.Context, version string, c config, security *peer.PeerTLS) error {
	for {
		err := serveGeneration(ctx, version, &c, security)
		if !errors.Is(err, errCertificateRenewed) || ctx.Err() != nil {
			return err
		}
		if c.Peer.seedSource == "certificate" {
			c.Peer.Seeds = nil
		}
		security, err = c.preparePeering()
		if err != nil {
			return fmt.Errorf("activate renewed certificate: %w", err)
		}
	}
}

func serveGeneration(ctx context.Context, version string, c *config, security *peer.PeerTLS) error {
	if c.StateDir == "" {
		var err error
		c.StateDir, err = defaultStateDir()
		if err != nil {
			return err
		}
	}
	shareAddresses := c.Peer.ShareAddresses == nil || *c.Peer.ShareAddresses
	readNode := func() (status.Node, error) {
		info, err := state.LocalNode(c.Peer.Advertise, c.Peer.Relay)
		if !shareAddresses {
			info.Addresses = []string{}
		}
		return info, err
	}
	info, err := readNode()
	if err != nil {
		return err
	}
	var trust state.RecordTrust
	if security != nil {
		trust = security
	}
	dir, err := state.OpenWithTrust(c.StateDir, info, trust)
	if err != nil {
		return err
	}
	defer dir.Close()
	if slices.Contains(c.Peer.DenyNodes, dir.Self().ID) {
		return fmt.Errorf("cannot deny the local node")
	}

	var mcpListener, peerListener net.Listener
	if c.MCPListen != "" {
		mcpListener, err = net.Listen("tcp", c.MCPListen)
		if err != nil {
			return fmt.Errorf("MCP listener: %w", err)
		}
		defer mcpListener.Close()
	}
	if c.Peer.Listen != "" {
		peerListener, err = net.Listen("tcp", c.Peer.Listen)
		if err != nil {
			return fmt.Errorf("peer listener: %w", err)
		}
		defer peerListener.Close()
	}
	// Retain assigned ports across in-process service restarts.
	if peerListener != nil {
		c.Peer.Listen = peerListener.Addr().String()
	}
	if mcpListener != nil {
		c.MCPListen = mcpListener.Addr().String()
	}
	var renewal peer.RenewalConfig
	if c.Peer.Renewal != nil {
		renewal = *c.Peer.Renewal
	}
	p := peer.Start(ctx, peer.Config{Renewal: renewal, CertFile: c.Peer.CertFile, TLS: security, Seeds: c.Peer.Seeds, DenyNodes: c.Peer.DenyNodes, TrustedRelays: c.Peer.TrustedRelays, HideInterfaceAddresses: !shareAddresses, Refresh: func() (bool, error) {
		info, err := readNode()
		if err != nil {
			return false, err
		}
		return dir.UpdateSelf(info)
	}}, peerListener, dir, status.Native(c.Filesystems, version), nil, nil)
	defer p.Close()
	fmt.Fprintf(os.Stderr, "host-status-mcp %s node_id=%s\n", version, dir.Self().ID)
	if peerListener != nil {
		fmt.Fprintln(os.Stderr, "peer listening on", peerListener.Addr())
	}
	if mcpListener == nil {
		select {
		case <-ctx.Done():
			return nil
		case <-p.Renewed():
			return errCertificateRenewed
		case <-p.Done():
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("peer service stopped")
		}
	}
	mcp := mcpserver.New(version, p)
	var leaf *x509.Certificate
	if security != nil {
		leaf = security.LocalCertificate()
	}
	runtimePeer := mcpserver.PeerSettings{
		Renewal: (*mcpserver.RenewalSettings)(c.Peer.Renewal), Security: c.Peer.Security, Advertise: c.Peer.Advertise, Seeds: c.Peer.Seeds,
		SeedSource: c.Peer.seedSource, ShareAddresses: shareAddresses, Relay: c.Peer.Relay,
		TrustedRelays: c.Peer.TrustedRelays, DenyNodes: c.Peer.DenyNodes,
	}
	if peerListener != nil {
		runtimePeer.Listen = peerListener.Addr().String()
	}
	mcpserver.AddServerConfig(mcp, mcpserver.ServerConfig{
		NodeID: dir.Self().ID, MCPTransport: "http", MCPListen: mcpListener.Addr().String(),
		StateDir: c.StateDir, Filesystems: c.Filesystems, AllowProcessKill: c.AllowProcessKill, Peer: runtimePeer,
	}, leaf)
	mcpserver.AddProcessStatus(mcp, dir.Self().ID)
	if c.AllowProcessKill {
		mcpserver.AddProcessKill(mcp, dir.Self().ID)
	}
	handler := mcpserver.HTTP(mcp)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	fmt.Fprintf(os.Stderr, "MCP listening on http://%s/mcp\n", mcpListener.Addr())
	done := make(chan error, 1)
	go func() { done <- server.Serve(mcpListener) }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	case <-p.Renewed():
		err = errCertificateRenewed
	case <-p.Done():
		if ctx.Err() == nil {
			server.Close()
			return fmt.Errorf("peer service stopped")
		}
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(stopCtx); err != nil {
		server.Close()
	}
	return err
}

func defaultStateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "host-status-mcp", "state"), nil
}
