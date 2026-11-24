package mcpserver

import (
	"context"
	"log/slog"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func AddProcessKill(server *mcp.Server, nodeID string) {
	destructive := true
	mcp.AddTool(server, &mcp.Tool{
		Name:        "process_kill",
		Description: "Send TERM (default) or KILL to one local Linux process. Use the PID and process_identity returned by the local processes list.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, req status.ProcessKillRequest) (*mcp.CallToolResult, status.ProcessKillResult, error) {
		result, err := status.KillProcess(ctx, nodeID, req)
		if err == nil {
			slog.Info("process signal sent", "node_id", nodeID, "pid", result.PID, "signal", result.Signal)
		}
		return nil, result, err
	})
}
