package mcpserver

import (
	"context"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type processStatusRequest struct {
	PID int32 `json:"pid" jsonschema:"Positive PID of a process local to this MCP server"`
}

func AddProcessStatus(server *mcp.Server, nodeID string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "process_status",
		Description: "Read one local process by PID, including its CPU usage sampled over about one second, resident memory and, when available, process_identity. Works for processes outside the host_status top 100.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, req processStatusRequest) (*mcp.CallToolResult, status.Process, error) {
		result, err := status.ReadProcess(ctx, nodeID, req.PID)
		return nil, result, err
	})
}
