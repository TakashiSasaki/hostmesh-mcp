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

	"github.com/TakashiSasaki/host-status-mcp/internal/mcpserver"
	"github.com/TakashiSasaki/host-status-mcp/internal/service"
	"github.com/TakashiSasaki/host-status-mcp/internal/state"
	"github.com/TakashiSasaki/host-status-mcp/internal/status"
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
	if len(args) > 0 && args[0] == "serve" {
		flags := flag.NewFlagSet("serve", flag.ContinueOnError)
		path := flags.String("config", "", "JSON configuration file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *path == "" || flags.NArg() != 0 {
			return fmt.Errorf("usage: host-status-mcp serve -config <path>")
		}
		cfg, err := readConfig(*path)
		if err != nil {
			return err
		}
		return serve(ctx, version, cfg)
	}
	if len(args) == 0 || args[0] != "stdio" {
		return fmt.Errorf("usage: host-status-mcp version | stdio [-state-dir directory] | serve -config <path>")
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
	store, err := state.Open(*dir, info)
	if err != nil {
		return err
	}
	defer store.Close()
	s := &service.Service{State: store, Collect: status.Native([]string{"/"}, version)}
	fmt.Fprintf(os.Stderr, "host-status-mcp %s node_id=%s\n", version, store.Self().ID)
	server := mcpserver.New(version, s)
	mcpserver.AddProcessStatus(server, store.Self().ID)
	mcpserver.AddProcessKill(server, store.Self().ID)
	return server.Run(ctx, &mcp.StdioTransport{})
}
