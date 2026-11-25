package main

import (
	"context"
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
	CAFile         string   `json:"ca_file,omitempty"`
	CertFile       string   `json:"cert_file,omitempty"`
	KeyFile        string   `json:"key_file,omitempty"`
	Security       string   `json:"security,omitempty"`
	Listen         string   `json:"listen,omitempty"`
	Advertise      []string `json:"advertise,omitempty"`
	Seeds          []string `json:"seeds,omitempty"`
	ShareAddresses *bool    `json:"share_addresses,omitempty"`
	Relay          bool     `json:"relay,omitempty"`
	TrustedRelays  []string `json:"trusted_relays,omitempty"`
	DenyNodes      []string `json:"deny_nodes,omitempty"`
}

func readConfig(path string) (config, error) {
	var c = config{MCPListen: "127.0.0.1:8787", Filesystems: []string{"/"}}
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	decoder := json.NewDecoder(io.LimitReader(f, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("decode configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration must contain one JSON object")
	}
	if c.MCPListen != "" {
		host, _, err := net.SplitHostPort(c.MCPListen)
		ip, ipErr := netip.ParseAddr(host)
		if err != nil || ipErr != nil || !ip.IsLoopback() {
			return c, fmt.Errorf("mcp_listen must be a loopback IP and port")
		}
	}
	if c.MCPListen == "" && c.Peer.Listen == "" && len(c.Peer.Seeds) == 0 {
		return c, fmt.Errorf("configure at least one MCP or peer endpoint")
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
	if c.Peer.Listen == "" && len(c.Peer.Advertise) != 0 {
		return c, fmt.Errorf("peer.advertise requires peer.listen")
	}
	if len(c.Peer.Advertise) > 16 || len(c.Peer.Seeds) > 16 {
		return c, fmt.Errorf("at most 16 advertised endpoints or seeds are supported")
	}
	for _, e := range slices.Concat(c.Peer.Advertise, c.Peer.Seeds) {
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

func serve(ctx context.Context, version string, c config) error {
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
	var security *peer.PeerTLS
	var trust state.RecordTrust
	if c.Peer.Security == "mtls" {
		security, err = peer.LoadPeerTLS(c.Peer.CAFile, c.Peer.CertFile, c.Peer.KeyFile)
		if err != nil {
			return err
		}
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
	p := peer.Start(ctx, peer.Config{TLS: security, Seeds: c.Peer.Seeds, DenyNodes: c.Peer.DenyNodes, TrustedRelays: c.Peer.TrustedRelays, HideInterfaceAddresses: !shareAddresses, Refresh: func() (bool, error) {
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
		case <-p.Done():
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("peer service stopped")
		}
	}
	mcp := mcpserver.New(version, p)
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
	return nil
}

func defaultStateDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "host-status-mcp", "state"), nil
}
