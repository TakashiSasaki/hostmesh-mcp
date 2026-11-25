package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/TakashiSasaki/host-status-mcp/internal/authority"
)

func runCA(ctx context.Context, args []string) error {
	const usage = "usage: host-status-mcp ca {serve|init|enroll|revoke|config check} -config <ca.json> (template: host-status-mcp ca config example)"
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "config" {
		if len(args) == 2 && args[1] == "example" {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(authority.Config{
				Listen: "127.0.0.1:9443", Roots: "root.pem",
				IssuerCert: "issuer.pem", IssuerKey: "issuer.key",
				ServerCert: "est-chain.pem", ServerKey: "est.key",
				StateDir: "state", Lifetime: "720h",
			})
		}
		if len(args) < 2 || args[1] != "check" {
			return errors.New(usage)
		}
		command = "config check"
		args = args[1:]
	}
	fs := flag.NewFlagSet("ca "+command, flag.ContinueOnError)
	configFile := fs.String("config", "", "CA JSON configuration")
	var profile, csrPath, node, certPath string
	switch command {
	case "init", "serve", "config check":
	case "enroll":
		fs.StringVar(&profile, "profile", "", "operator-approved node profile JSON")
		fs.StringVar(&csrPath, "csr", "", "signed PEM CSR from the node")
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
	case "enroll":
		if csrPath == "" || profile == "" {
			return errors.New("ca enroll requires -csr and -profile")
		}
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
	if command == "serve" {
		return a.Serve(ctx, os.Stderr)
	}
	p, err := authority.ReadProfile(profile)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(csrPath)
	if err != nil {
		return err
	}
	chain, err := a.Enroll(p, data)
	if err != nil {
		return err
	}
	if _, err = os.Stdout.Write(chain); err != nil {
		return fmt.Errorf("write issued certificate: %w", err)
	}
	return nil
}
