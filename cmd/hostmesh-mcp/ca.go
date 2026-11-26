package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
)

func runCA(ctx context.Context, args []string) error {
	const usage = "usage: hostmesh-mcp ca <command> (see hostmesh-mcp help ca or hostmesh-mcp help enrollment)"
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "inspect" || command == "enroll" {
		return runEnrollment(command, args[1:])
	}
	if command == "bootstrap" {
		fs := flag.NewFlagSet("ca bootstrap", flag.ContinueOnError)
		dir := fs.String("dir", "", "new directory for offline and online CA files")
		name := fs.String("server-name", "", "DNS name or IP address in the peers' EST renewal URL")
		if err := fs.Parse(args[1:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if *dir == "" || *name == "" || fs.NArg() != 0 {
			return errors.New(usage)
		}
		if err := authority.Bootstrap(*dir, *name); err != nil {
			return err
		}
		fmt.Printf("CA config: %s\nOffline root key: %s\n", filepath.Join(*dir, "online", "ca.json"), filepath.Join(*dir, "offline", "root.key"))
		return nil
	}
	if command == "config" {
		if len(args) == 2 && args[1] == "example" {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(authority.ExampleConfig())
		}
		if len(args) < 2 || args[1] != "check" {
			return errors.New(usage)
		}
		command = "config check"
		args = args[1:]
	}
	fs := flag.NewFlagSet("ca "+command, flag.ContinueOnError)
	configFile := fs.String("config", "", "CA JSON configuration")
	var node, certPath string
	switch command {
	case "init", "serve", "config check":
	case "revoke":
		fs.StringVar(&certPath, "cert", "", "revoke exactly the key of this PEM peer certificate")
		fs.StringVar(&node, "node-id", "", "revoke this node's registered public key")
	default:
		return errors.New(usage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configFile == "" || fs.NArg() != 0 {
		return errors.New(usage)
	}
	cfg, err := authority.ReadConfig(*configFile)
	if err != nil {
		return err
	}
	switch command {
	case "init":
		return authority.Init(cfg.StateDir)
	case "revoke":
		if (node == "") == (certPath == "") {
			return errors.New("ca revoke requires exactly one of -node-id or -cert")
		}
		var cert []byte
		if certPath != "" {
			cert, err = os.ReadFile(certPath)
			if err != nil {
				return err
			}
		}
		return authority.Revoke(cfg.StateDir, node, cert)
	}
	a, err := authority.Load(cfg)
	if err != nil {
		return err
	}
	if command == "config check" {
		if err := a.Check(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "CA configuration OK")
		return nil
	}
	return a.Serve(ctx, os.Stderr)
}
