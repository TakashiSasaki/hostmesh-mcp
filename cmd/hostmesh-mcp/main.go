package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/mcpserver"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/service"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/state"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed VERSION
var versionFile string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	version := strings.TrimSpace(versionFile)
	if len(args) == 1 && args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if len(args) > 0 && args[0] == "ca" {
		return runCA(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "supervise" {
		return runSupervise(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "update" {
		return runUpdate(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "config" {
		if len(args) >= 2 {
			switch args[1] {
			case "example":
				return configExample(args[2:], os.Stdout)
			case "check":
				return runConfigured(ctx, version, "config check", args[2:])
			}
		}
		return fmt.Errorf("usage: hostmesh-mcp config example [-join] [-mtls] | config check -config <path>")
	}
	if len(args) > 0 && args[0] == "serve" {
		return runConfigured(ctx, version, "serve", args[1:])
	}
	if len(args) == 0 || args[0] != "stdio" {
		return fmt.Errorf("usage: hostmesh-mcp version | ca {bootstrap|serve|init|enroll|revoke|config} | update {check|apply} -config <path> | supervise -config <updater.json> | stdio [-state-dir directory] | serve -config <path> | config example [-join] [-mtls] | config check -config <path>")
	}
	flags := flag.NewFlagSet("stdio", flag.ContinueOnError)
	dir := flags.String("state-dir", "", "persistent state directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if *dir == "" {
		stateDir, err := defaultStateDir()
		if err != nil {
			return err
		}
		*dir = stateDir
	}
	info, err := state.LocalNode(nil, false)
	if err != nil {
		return err
	}
	store, err := state.OpenLocal(*dir, info)
	if err != nil {
		return err
	}
	defer store.Close()
	filesystems := []string{defaultFilesystemRoot()}
	s := &service.Service{State: store, Collect: status.Native(filesystems, version)}
	fmt.Fprintf(os.Stderr, "hostmesh-mcp %s node_id=%s\n", version, store.Self().ID)
	server := mcpserver.New(version, s)
	allowKill := mcpserver.RegisterProcessKill(server, store.Self().ID, true)
	mcpserver.AddServerConfig(server, mcpserver.ServerConfig{
		NodeID: store.Self().ID, MCPTransport: "stdio", StateDir: *dir, Filesystems: filesystems,
		AllowProcessKill: allowKill, Peer: mcpserver.PeerSettings{Security: "disabled", SeedSource: "none"},
	}, nil)
	mcpserver.AddProcessStatus(server, store.Self().ID)
	return server.Run(ctx, &mcp.StdioTransport{})
}

func runConfigured(ctx context.Context, version, command string, args []string) error {
	flags := flag.NewFlagSet("hostmesh-mcp "+command, flag.ContinueOnError)
	path := flags.String("config", "", "JSON configuration file")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return fmt.Errorf("usage: hostmesh-mcp %s -config <path>\nGenerate a template with: hostmesh-mcp config example > peer.json", command)
	}

	cfg, err := readConfig(*path)
	if err != nil {
		return fmt.Errorf("configuration %q: %w", *path, err)
	}
	security, err := cfg.preparePeering()
	if err != nil {
		return fmt.Errorf("configuration %q: %w", *path, err)
	}
	if command == "config check" {
		_, err := fmt.Fprintf(os.Stdout, "Configuration OK (peer mode: %s, seeds: %d, source: %s)\n", cfg.Peer.Security, len(cfg.Peer.Seeds), cfg.Peer.seedSource)
		return err
	}
	return serve(ctx, version, cfg, security)
}
