package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

func configExample(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("hostmesh-mcp config example", flag.ContinueOnError)
	join := flags.Bool("join", false, "use peer.seeds to dial an existing peer")
	mtls := flags.Bool("mtls", false, "include ca_file (trusted CA), cert_file (this node's certificate) and key_file (its private key)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: hostmesh-mcp config example [-join] [-mtls]")
		fmt.Fprintln(flags.Output(), "Print an editable JSON configuration. By default peer.listen binds a local address and peer.advertise publishes it. Replace loopback addresses when connecting different hosts.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q (see hostmesh-mcp config example -help)", flags.Arg(0))
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(peerExample(*join, *mtls))
}

func peerExample(join, mtls bool) map[string]any {
	const endpoint = "127.0.0.1:7443"
	var peer peerConfig
	if join {
		peer.Seeds = []string{endpoint}
	} else {
		peer.Listen = endpoint
		peer.Advertise = []string{endpoint}
	}
	if mtls {
		peer.CAFile = "/etc/hostmesh-mcp/ca.pem"
		peer.CertFile = "/etc/hostmesh-mcp/node.pem"
		peer.KeyFile = "/etc/hostmesh-mcp/node.key"
	}
	return map[string]any{"mcp_listen": "127.0.0.1:8787", "peer": peer}
}
