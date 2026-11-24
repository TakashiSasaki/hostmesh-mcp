package mcpserver

import (
	"context"
	"log/slog"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type processStatusRequest struct {
	PID int32 `json:"pid" jsonschema:"Positive PID of a process local to this MCP server"`
}

func AddProcessStatus(server *mcp.Server, nodeID string) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "process_status",
		Description: "Read one local Linux process by PID, including its CPU usage sampled over about one second, resident memory and process_identity. Works for processes outside the host_status top 100.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, req processStatusRequest) (*mcp.CallToolResult, status.Process, error) {
		result, err := status.ReadProcess(ctx, nodeID, req.PID)
		return nil, result, err
	})
}

func AddProcessKill(server *mcp.Server, nodeID string) {
	destructive := true
	mcp.AddTool(server, &mcp.Tool{
		Name:        "process_kill",
		Description: "Send TERM (default) or KILL to one local Linux process. Use the PID and process_identity returned by process_status or the local processes list.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, req status.ProcessKillRequest) (*mcp.CallToolResult, status.ProcessKillResult, error) {
		result, err := status.KillProcess(ctx, nodeID, req)
		if err == nil {
			slog.Info("process signal sent", "node_id", nodeID, "pid", result.PID, "signal", result.Signal)
		}
		return nil, result, err
	})
}
