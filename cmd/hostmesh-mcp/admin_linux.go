package main

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (a *adminServer) addLinux(s *mcp.Server) {
	a.addUnits(s)
	type updateExample struct {
		Supervised bool `json:"supervised,omitempty" jsonschema:"Generate settings for an init-supervised peer instead of an installed systemd unit"`
	}
	adminTool(s, "update_config_example", "Generate Linux updater settings for GitHub Releases. Replace the absolute peer binary path and service unit or peer config before config_save.", true, false, func(_ context.Context, in updateExample) (adminResult, error) {
		config := adminResult{"source": "github", "binary": "/opt/hostmesh/hostmesh-mcp"}
		if in.Supervised {
			config["peer_config"] = a.peerPath
			config["check_interval"] = "24h"
		} else {
			unit := a.peerUnit
			if unit == "" {
				unit = "hostmesh-mcp.service"
			}
			config["systemd_unit"] = unit
			config["user_service"] = a.userUnits
		}
		return adminResult{"config": config}, nil
	})
	for _, operation := range []string{"check", "apply"} {
		check := operation == "check"
		description := "Check available and installed binary versions using the bound Linux updater configuration."
		if !check {
			description = "Apply a systemd peer update after release verification. For a session-owned supervisor use supervisor_update instead."
		}
		adminTool(s, "update_"+operation, description, check, !check, func(ctx context.Context, _ noArgs) (adminResult, error) {
			var out bytes.Buffer
			if err := performUpdate(ctx, a.updatePath, check, &out); err != nil {
				return nil, err
			}
			return adminResult{"report": out.String()}, nil
		})
	}
	adminTool(s, "supervisor_start", "Start the Linux supervisor using the bound updater configuration. Running indicates the supervisor acquired its lock; the peer starts and retries independently.", false, false, func(ctx context.Context, _ noArgs) (adminResult, error) {
		return a.services.start(ctx, "supervisor", func(child context.Context, ready func(adminResult), trigger <-chan os.Signal) error {
			return superviseReady(child, a.updatePath, trigger, os.Stderr, func() { ready(adminResult{"updater_config": a.updatePath}) })
		})
	})
	adminTool(s, "supervisor_update", "Request an immediate update check and application by the session-owned Linux supervisor. The request is asynchronous.", false, true, func(context.Context, noArgs) (adminResult, error) {
		a.services.mu.Lock()
		defer a.services.mu.Unlock()
		r := a.services.runs["supervisor"]
		if r == nil || r.status() != "running" {
			return nil, errors.New("start the supervisor with supervisor_start first")
		}
		select {
		case r.update <- nil:
		default:
		}
		return adminResult{"status": "requested"}, nil
	})
}

type unitTarget struct {
	Service string `json:"service" jsonschema:"peer or ca as bound by the administrator at startup"`
}

func (a *adminServer) addUnits(s *mcp.Server) {
	if a.peerUnit == "" && a.caUnit == "" {
		return
	}
	for _, action := range []string{"status", "start", "stop", "restart"} {
		adminTool(s, "unit_"+action, "Systemd "+action+" for a preinstalled unit bound at launch. The service lifetime is independent of this administration session.", action == "status", action == "stop" || action == "restart", func(ctx context.Context, in unitTarget) (adminResult, error) {
			var unit string
			switch in.Service {
			case "peer":
				unit = a.peerUnit
			case "ca":
				unit = a.caUnit
			}
			if unit == "" {
				return nil, errors.New("service must name a bound unit; check admin_info")
			}
			manager := updateTarget{Unit: unit, User: a.userUnits}
			if action != "status" {
				if _, err := manager.systemctl(ctx, action); err != nil {
					return nil, err
				}
			}
			state, err := manager.systemctl(ctx, "show", "--value", "--property=ActiveState")
			if err != nil {
				return nil, err
			}
			return adminResult{"service": in.Service, "unit": unit, "active_state": state}, nil
		})
	}
}
