package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/authority"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/mcpserver"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type adminResult map[string]any
type noArgs struct{}
type adminServer struct {
	peerPath, caPath, updatePath, version string
	peerUnit, caUnit                      string
	userUnits                             bool
	configMu                              sync.Mutex
	services                              adminServices
}

func runAdmin(ctx context.Context, version, mode string, args []string) error {
	a := adminServer{version: version}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.StringVar(&a.peerPath, "peer-config", "peer.json", "managed peer configuration")
	fs.StringVar(&a.caPath, "ca-config", filepath.Join("mesh-ca", "online", "ca.json"), "managed CA configuration")
	fs.StringVar(&a.updatePath, "update-config", "updater.json", "managed Linux updater configuration")
	if runtime.GOOS == "linux" {
		fs.StringVar(&a.peerUnit, "peer-unit", "", "existing systemd peer or supervisor unit managed across sessions")
		fs.StringVar(&a.caUnit, "ca-unit", "", "existing systemd CA unit managed across sessions")
		fs.BoolVar(&a.userUnits, "user-units", false, "use the current user systemd manager for bound units")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if mode == "mcp" && fs.NArg() != 0 {
		return errors.New(helpTopics["mcp"])
	}
	for _, unit := range []string{a.peerUnit, a.caUnit} {
		if unit != "" && !unitName.MatchString(unit) {
			return fmt.Errorf("invalid systemd service unit %q", unit)
		}
	}
	paths := map[string]bool{}
	for _, path := range []*string{&a.peerPath, &a.caPath, &a.updatePath} {
		if *path == "" {
			return errors.New("configuration path must not be empty")
		}
		absolute, err := filepath.Abs(*path)
		if err != nil {
			return err
		}
		if paths[absolute] {
			return errors.New("each configuration requires a separate file")
		}
		*path = absolute
		paths[absolute] = true
	}
	a.services.init(ctx)
	defer a.services.close()
	server := mcp.NewServer(&mcp.Implementation{Name: "hostmesh-mcp-admin", Version: version}, &mcp.ServerOptions{Instructions: "Local hostmesh administration. Start with admin_help for the workflow and admin_info for configuration paths. Use peer_csr, ca_inspect_csr, ca_enroll and peer_install_certificate for enrollment. Only submit a fingerprint independently approved by the operator. Service lifetimes are scoped to this administration session."})
	adminTool(server, "admin_info", "Read the version, operating system and bound configuration paths.", true, false, func(context.Context, noArgs) (adminResult, error) {
		return adminResult{"version": version, "os": runtime.GOOS, "peer_config": a.peerPath, "ca_config": a.caPath, "update_config": a.updatePath, "peer_unit": a.peerUnit, "ca_unit": a.caUnit, "user_units": a.userUnits}, nil
	})
	adminTool(server, "admin_help", "Read the task guide for first enrollment, service lifecycle, configuration and Linux updates.", true, false, func(context.Context, noArgs) (adminResult, error) {
		return adminResult{"guide": helpTopics["administration"]}, nil
	})
	a.addConfigurations(server)
	a.addCertificates(server)
	a.addServices(server)
	a.addLinux(server)
	const nodeID = "hostmesh-mcp-admin" // Linux additionally scopes identity to boot ID and PID namespace.
	mcpserver.AddProcessStatus(server, nodeID)
	mcpserver.RegisterProcessKill(server, nodeID, true)
	if mode == "admin" {
		return adminCLI(ctx, server, fs.Args())
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func adminTool[I any](s *mcp.Server, name, description string, readOnly, destructive bool, f func(context.Context, I) (adminResult, error)) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive}}, func(ctx context.Context, _ *mcp.CallToolRequest, in I) (*mcp.CallToolResult, adminResult, error) {
		out, err := f(ctx, in)
		return nil, out, err
	})
}

func (a *adminServer) loadCA() (*authority.Authority, error) {
	cfg, err := authority.ReadConfig(a.caPath)
	if err != nil {
		return nil, err
	}
	return authority.Load(cfg)
}

type configKind struct {
	Kind string `json:"kind" jsonschema:"peer or ca; updater is also supported on Linux"`
}

func (a *adminServer) configPath(kind string) (string, error) {
	switch kind {
	case "peer":
		return a.peerPath, nil
	case "ca":
		return a.caPath, nil
	case "updater":
		if runtime.GOOS == "linux" {
			return a.updatePath, nil
		}
	}
	return "", fmt.Errorf("unsupported configuration kind %q on %s", kind, runtime.GOOS)
}

func checkAdminConfig(kind, path string) error {
	switch kind {
	case "peer":
		cfg, err := readConfig(path)
		if err != nil {
			return err
		}
		_, err = cfg.preparePeering()
		return err
	case "ca":
		cfg, err := authority.ReadConfig(path)
		if err != nil {
			return err
		}
		ca, err := authority.Load(cfg)
		if err != nil {
			return err
		}
		return ca.Check()
	default:
		_, err := readUpdateSettings(path)
		return err
	}
}

func (a *adminServer) addConfigurations(s *mcp.Server) {
	type example struct {
		Join bool `json:"join,omitempty" jsonschema:"Generate an outbound peer with initial seeds"`
		MTLS bool `json:"mtls,omitempty" jsonschema:"Include CA certificate and key file settings"`
	}
	adminTool(s, "peer_config_example", "Generate editable peer settings. Top-level mcp_listen is the HTTP listener (127.0.0.1:0 requests an available port); peer.listen is the mesh listener. Set a unique state_dir for concurrent peers. Install via config_save.", true, false, func(_ context.Context, in example) (adminResult, error) {
		return adminResult{"config": peerExample(in.Join, in.MTLS)}, nil
	})
	adminTool(s, "ca_config_example", "Generate editable EST authority settings.", true, false, func(context.Context, noArgs) (adminResult, error) {
		return adminResult{"config": authority.ExampleConfig()}, nil
	})
	adminTool(s, "config_check", "Validate the bound configuration and configured credentials without starting a service.", true, false, func(_ context.Context, in configKind) (adminResult, error) {
		path, err := a.configPath(in.Kind)
		if err != nil {
			return nil, err
		}
		if err = checkAdminConfig(in.Kind, path); err != nil {
			return nil, err
		}
		return adminResult{"valid": true, "path": path}, nil
	})
	adminTool(s, "config_read", "Read a bound configuration JSON object and its SHA-256 revision for config_save.", true, false, a.readConfiguration)
	adminTool(s, "config_save", "Validate and atomically install a configuration for a subsequent start. Omit expected_sha256 for a new file; when editing use the revision from config_read.", false, true, a.saveConfiguration)
}
