package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	if len(args) == 0 || args[0] != "stdio" {
		return fmt.Errorf("usage: host-status-mcp version | stdio [-state-dir directory]")
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
		root, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		*dir = filepath.Join(root, "host-status-mcp", "state")
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
	s := &service.Service{State: store, Collect: status.Native([]string{"/"})}
	fmt.Fprintf(os.Stderr, "host-status-mcp %s node_id=%s\n", version, store.Self().ID)
	return mcpserver.New(version, s).Run(ctx, &mcp.StdioTransport{})
}
