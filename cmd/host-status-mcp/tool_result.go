package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolResult(ctx context.Context, s *mcp.ClientSession, name string, args any, dst any) error {
	r, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if r.IsError {
		return fmt.Errorf("%s returned an error", name)
	}
	b, err := json.Marshal(r.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
