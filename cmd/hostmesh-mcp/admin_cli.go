package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These operations span multiple calls on one live management connection.
var cliServiceAlternatives = map[string]string{
	"peer_start":        "serve -config <peer.json>",
	"ca_start":          "ca serve -config <ca.json>",
	"supervisor_start":  "supervise -config <updater.json>",
	"service_status":    "use the OS service manager or bind a Linux unit and call unit_status",
	"service_stop":      "stop the foreground process or call unit_stop for a bound Linux unit",
	"supervisor_update": "send SIGUSR1 to the running Linux supervisor",
}

// Use the published schemas and the same validated handlers from both interfaces.
func adminCLI(ctx context.Context, s *mcp.Server, args []string) error {
	if len(args) == 0 {
		return errors.New(helpTopics["admin"])
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := s.Connect(ctx, serverTransport, nil)
	if err != nil {
		return err
	}
	defer server.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "hostmesh-mcp-cli", Version: strings.TrimSpace(versionFile)}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		return err
	}
	defer client.Close()
	out := json.NewEncoder(os.Stdout)
	out.SetIndent("", "  ")
	switch args[0] {
	case "list", "describe":
		describe := args[0] == "describe"
		if describe && len(args) != 2 || !describe && len(args) != 1 {
			return errors.New(helpTopics["admin"])
		}
		for tool, err := range client.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			if describe {
				if args[1] == tool.Name {
					return out.Encode(tool)
				}
			} else {
				description := tool.Description
				if alternative, ok := cliServiceAlternatives[tool.Name]; ok {
					description = "Persistent MCP session tool. CLI: " + alternative
				}
				fmt.Fprintf(os.Stdout, "%-26s %s\n", tool.Name, description)
			}
		}
		if describe {
			return fmt.Errorf("unknown tool %q (see hostmesh-mcp admin list)", args[1])
		}
		return nil
	case "call":
		if len(args) < 2 || len(args) > 3 {
			return errors.New(helpTopics["admin"])
		}
		if alternative, ok := cliServiceAlternatives[args[1]]; ok {
			return fmt.Errorf("%s needs a persistent MCP session. CLI: %s", args[1], alternative)
		}
		var input map[string]any
		if len(args) == 3 {
			var reader io.Reader = strings.NewReader(args[2])
			if args[2] == "-" {
				reader = os.Stdin
			}
			decoder := json.NewDecoder(reader)
			decoder.UseNumber()
			if err := decoder.Decode(&input); err != nil {
				return err
			}
			if input == nil || decoder.Decode(new(any)) != io.EOF {
				return errors.New("expected one JSON object")
			}
		} else {
			input = map[string]any{}
		}
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: args[1], Arguments: input})
		if err != nil {
			return err
		}
		if result.IsError {
			for _, item := range result.Content {
				if msg, ok := item.(*mcp.TextContent); ok {
					return errors.New(msg.Text)
				}
			}
			return fmt.Errorf("%s failed", args[1])
		}
		return out.Encode(result.StructuredContent)
	default:
		return errors.New(helpTopics["admin"])
	}
}
