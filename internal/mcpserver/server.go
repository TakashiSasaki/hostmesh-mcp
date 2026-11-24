package mcpserver

import (
	"context"
	"errors"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Backend interface {
	List() []status.Host
	Get(context.Context, status.Query) (status.Result, error)
}

type hostList struct {
	Hosts []status.Host `json:"hosts"`
}

type serverVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func New(version string, backend Backend) *mcp.Server {
	const serverName = "host-status-mcp"
	server := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: version}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "server_version", Description: "Get the version of the connected MCP server.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, serverVersion, error) {
			return nil, serverVersion{Name: serverName, Version: version}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "host_list", Description: "List known hosts and their connection state.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, hostList, error) {
			return nil, hostList{Hosts: backend.List()}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "host_status", Description: "Read host status. Omit target for the local host. Select system, cpu, memory, filesystems, swap, network or processes. Processes reports the top 100 by sampled CPU usage.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, q status.Query) (*mcp.CallToolResult, status.Result, error) {
			result, err := backend.Get(ctx, q)
			if err != nil {
				switch {
				case errors.Is(err, context.DeadlineExceeded):
					err = status.Error("DEADLINE_EXCEEDED", "status request timed out")
				case errors.Is(err, context.Canceled):
					err = status.Error("CANCELED", "status request was canceled")
				}
			}
			return nil, result, err
		})
	return server
}
