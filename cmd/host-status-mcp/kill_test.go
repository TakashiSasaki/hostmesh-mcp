//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioProcessKill(t *testing.T) {
	for _, signal := range []string{"TERM", "KILL"} {
		t.Run(signal, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := appCommand(ctx, "stdio", "-state-dir", t.TempDir())
			client := mcp.NewClient(&mcp.Implementation{Name: "kill-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			worker := exec.CommandContext(ctx, "sleep", "30")
			if err := worker.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- worker.Wait()
			}()
			defer func() {
				worker.Process.Kill()
				<-done
			}()

			r, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "process_status", Arguments: map[string]any{
				"pid": worker.Process.Pid,
			}})
			if err != nil || r.IsError {
				t.Fatalf("process_status = %+v, %v", r, err)
			}
			var process status.Process
			data, err := json.Marshal(r.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &process); err != nil {
				t.Fatal(err)
			}
			if int(process.PID) != worker.Process.Pid || process.Identity == "" || process.CPUPercent == nil {
				t.Fatalf("missing worker details: %+v", process)
			}

			call := func(identity string) (*mcp.CallToolResult, error) {
				args := map[string]any{"pid": worker.Process.Pid, "process_identity": identity}
				if signal == "KILL" {
					args["signal"] = signal
				}
				return session.CallTool(ctx, &mcp.CallToolParams{Name: "process_kill", Arguments: args})
			}
			r, err = call(strings.Repeat("0", 64))
			if err != nil || !r.IsError || len(r.Content) == 0 {
				t.Fatalf("mismatched identity was not rejected: %+v, %v", r, err)
			}
			message, ok := r.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(message.Text, "PROCESS_IDENTITY_MISMATCH") {
				t.Fatalf("wrong identity error: %+v", r.Content)
			}

			r, err = call(process.Identity)
			if err != nil || r.IsError {
				t.Fatalf("process_kill = %+v, %v", r, err)
			}
			result, ok := r.StructuredContent.(map[string]any)
			if !ok || result["signal"] != signal || result["signal_sent"] != true {
				t.Fatalf("unexpected signal result: %+v", r.StructuredContent)
			}
			select {
			case err := <-done:
				var exit *exec.ExitError
				want := syscall.SIGTERM
				if signal == "KILL" {
					want = syscall.SIGKILL
				}
				if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != want {
					t.Fatalf("worker did not exit from %s: %v", signal, err)
				}
			case <-ctx.Done():
				t.Fatal("worker did not exit after signal")
			}
		})
	}
}

func TestHTTPProcessKillOptIn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
	}{{"default", false}, {"enabled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			run := startServeProcess(t, config{MCPListen: "127.0.0.1:0", AllowProcessKill: tc.allow})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := mcp.NewClient(&mcp.Implementation{Name: "kill-test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: run.mcp, HTTPClient: http.DefaultClient}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			found, readOnly := false, false
			for _, tool := range tools.Tools {
				found = found || tool.Name == "process_kill"
				if tool.Name == "process_status" {
					readOnly = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
				}
			}
			if !readOnly {
				t.Fatal("HTTP process_status is missing or not read only")
			}
			if found != tc.allow {
				t.Fatalf("process_kill advertised=%t, want %t", found, tc.allow)
			}
		})
	}
}
