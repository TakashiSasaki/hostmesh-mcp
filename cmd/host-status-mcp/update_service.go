package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These values are captured before shutdown and persisted with the rollback binary.
type updateTarget struct {
	OwnerUID   int
	PeerConfig string `json:",omitempty"`
	Unit       string
	User       bool
	StateDir   string
	NodeID     string
	Previous   string
	Endpoint   string
	Observer   string
}

func commandOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	data, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, bytes.TrimSpace(data))
	}
	return strings.TrimSpace(string(data)), nil
}

func (t updateTarget) systemctl(ctx context.Context, args ...string) (string, error) {
	if t.User {
		args = append([]string{"--user"}, args...)
	}
	return commandOutput(ctx, "", "systemctl", append(args, "--", t.Unit)...)
}

// Both service managers use the same update transaction and health checks.
type peerController interface {
	processDir(context.Context, string) (string, error)
	stop(context.Context) error
	start(context.Context, bool) error
}

func (t updateTarget) stop(ctx context.Context) error {
	_, err := t.systemctl(ctx, "stop")
	return err
}

func (t updateTarget) start(ctx context.Context, recovering bool) error {
	if recovering {
		// A stopped unit may already have been garbage-collected.
		_, _ = t.systemctl(ctx, "reset-failed")
	}
	_, err := t.systemctl(ctx, "start")
	return err
}

func (t updateTarget) processDir(ctx context.Context, binary string) (string, error) {
	pid, err := t.systemctl(ctx, "show", "--property=MainPID", "--value")
	if err != nil {
		return "", err
	}
	if pid == "" || pid == "0" || strings.Trim(pid, "0123456789") != "" {
		return "", fmt.Errorf("no MainPID for %s", t.Unit)
	}
	return checkedProcessDir(pid, binary)
}

func checkedProcessDir(pid, binary string) (string, error) {
	proc := filepath.Join("/proc", pid)
	running, err := os.Stat(filepath.Join(proc, "exe"))
	if err != nil {
		return "", err
	}
	expected, err := os.Stat(binary)
	if err != nil {
		return "", err
	}
	if !os.SameFile(expected, running) {
		return "", fmt.Errorf("process %s does not run %s", pid, binary)
	}
	return proc, nil
}

func inspectTarget(ctx context.Context, c updateSettings, manager peerController, previous, candidate string) (updateTarget, error) {
	t := updateTarget{OwnerUID: os.Geteuid(), Unit: c.Unit, User: c.UserService, Previous: previous, Observer: c.Observer, PeerConfig: c.PeerConfig}
	proc, err := manager.processDir(ctx, c.Binary)
	if err != nil {
		return t, err
	}
	// The service supplies its working directory and open ledger path. Never
	// interpret a lower-privileged process's filesystem inputs as root.
	status, err := os.ReadFile(filepath.Join(proc, "status"))
	if err != nil {
		return t, err
	}
	var uid string
	for line := range strings.SplitSeq(string(status), "\n") {
		if fields := strings.Fields(line); len(fields) == 5 && fields[0] == "Uid:" {
			uid = fields[2]
			break
		}
	}
	if uid != fmt.Sprint(os.Geteuid()) {
		return t, errors.New("run update apply as the service account")
	}
	raw, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		return t, err
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(args) < 3 || args[1] != "serve" {
		return t, errors.New("update requires a running serve process")
	}
	fs := flag.NewFlagSet("running serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	conf := fs.String("config", "", "")
	if err := fs.Parse(args[2:]); err != nil || *conf == "" || fs.NArg() != 0 {
		return t, errors.New("cannot determine the running serve configuration")
	}
	cwd, err := os.Readlink(filepath.Join(proc, "cwd"))
	if err != nil {
		return t, err
	}
	path := *conf
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	cfg, err := readConfig(path)
	if err != nil {
		return t, err
	}
	if _, err := commandOutput(ctx, cwd, candidate, "config", "check", "-config", path); err != nil {
		return t, err
	}
	for _, addr := range []string{cfg.MCPListen, cfg.Peer.Listen} {
		if addr == "" {
			continue
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return t, err
		}
		n, err := net.LookupPort("tcp", port)
		if err != nil || n == 0 {
			return t, errors.New("automatic update requires fixed listening ports")
		}
	}
	if cfg.MCPListen != "" {
		t.Endpoint = "http://" + cfg.MCPListen + "/mcp"
	}
	if t.Endpoint == "" && t.Observer == "" {
		return t, errors.New("set observer_mcp_url for a peer-only service")
	}
	// Use the lock actually held by the service, including its user-specific default.
	fds, err := os.ReadDir(filepath.Join(proc, "fd"))
	if err != nil {
		return t, err
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(proc, "fd", fd.Name()))
		if err == nil && filepath.IsAbs(target) && filepath.Base(target) == "state.lock" {
			if t.StateDir != "" {
				return t, errors.New("ambiguous state.lock files")
			}
			t.StateDir = filepath.Dir(target)
		}
	}
	if t.StateDir == "" {
		return t, errors.New("cannot locate running peer ledger")
	}
	if cfg.StateDir != "" {
		configured := cfg.StateDir
		if !filepath.IsAbs(configured) {
			configured = filepath.Join(cwd, configured)
		}
		runningLock, err := os.Stat(filepath.Join(t.StateDir, "state.lock"))
		if err != nil {
			return t, err
		}
		configuredLock, err := os.Stat(filepath.Join(configured, "state.lock"))
		if err != nil || !os.SameFile(runningLock, configuredLock) {
			return t, errors.New("configured state_dir differs from the running peer ledger")
		}
	}
	b, err := os.ReadFile(filepath.Join(t.StateDir, "state.json"))
	if err != nil {
		return t, err
	}
	var identity struct {
		ID string `json:"self_id"`
	}
	if err := json.Unmarshal(b, &identity); err != nil {
		return t, err
	}
	if uuid.Validate(identity.ID) != nil {
		return t, errors.New("invalid stored node ID")
	}
	t.NodeID = identity.ID
	return t, t.probe(ctx, previous)
}

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

func (t updateTarget) probe(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, observer := range []bool{false, true} {
		endpoint := t.Endpoint
		if observer {
			endpoint = t.Observer
		}
		if endpoint == "" {
			continue
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "host-status-updater", Version: strings.TrimSpace(versionFile)}, nil)
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
		if err != nil {
			return err
		}
		defer s.Close()
		if observer {
			var r struct {
				Node struct {
					ID string `json:"node_id"`
				} `json:"node"`
				Sections struct {
					System struct {
						Version string `json:"server_version"`
					} `json:"system"`
				} `json:"sections"`
			}
			if err := toolResult(ctx, s, "host_status", map[string]any{"target": map[string]string{"node_id": t.NodeID}, "sections": []string{"system"}}, &r); err != nil {
				return err
			}
			if r.Node.ID != t.NodeID || r.Sections.System.Version != version {
				return errors.New("unexpected node or version at observer")
			}
		} else {
			var v struct {
				Version string `json:"version"`
			}
			var node struct {
				ID string `json:"node_id"`
			}
			if err := toolResult(ctx, s, "server_version", struct{}{}, &v); err != nil {
				return err
			}
			if err := toolResult(ctx, s, "server_config", struct{}{}, &node); err != nil {
				return err
			}
			if node.ID != t.NodeID || v.Version != version {
				return errors.New("unexpected node or version at MCP endpoint")
			}
		}
	}
	return nil
}

func (t updateTarget) startAndVerify(ctx context.Context, manager peerController, binary, version string, recovering bool) error {
	if err := manager.start(ctx, recovering); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		_, err := manager.processDir(ctx, binary)
		if errors.Is(err, os.ErrProcessDone) {
			return err
		}
		if err == nil {
			err = t.probe(ctx, version)
		}
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("verify %s after start: %w", version, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
