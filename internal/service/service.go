package service

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/state"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
)

type Service struct {
	State   *state.Store
	Collect status.Collector
}

func (s *Service) List() []status.Host {
	now := time.Now().UTC()
	return []status.Host{{Node: s.State.Self(), IsLocal: true, Connection: "connected", LastSeenAt: &now}}
}

func (s *Service) Get(ctx context.Context, q status.Query) (status.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sections, err := status.ValidateSections(q.Sections)
	if err != nil {
		return status.Result{}, err
	}
	node, err := Resolve(s.State.Self(), s.List(), q.Target)
	if err != nil {
		return status.Result{}, err
	}
	return s.Collect(ctx, node, sections)
}

func Resolve(self status.Node, hosts []status.Host, target *status.Target) (status.Node, error) {
	if target == nil {
		return self, nil
	}
	if (target.ID != "" && (target.Hostname != "" || target.IP != "")) || (target.ID == "" && target.Hostname == "") {
		return status.Node{}, status.Error("INVALID_ARGUMENT", "specify node_id or hostname with optional ip")
	}
	var ip netip.Addr
	if target.IP != "" {
		var err error
		ip, err = netip.ParseAddr(target.IP)
		if err != nil || ip.Zone() != "" {
			return status.Node{}, status.Error("INVALID_ARGUMENT", "invalid IP address")
		}
		ip = ip.Unmap()
	}
	var matches []status.Node
	for _, host := range hosts {
		if target.ID != "" {
			if host.ID == target.ID {
				return host.Node, nil
			}
			continue
		}
		if !strings.EqualFold(host.Hostname, target.Hostname) {
			continue
		}
		if ip.IsValid() {
			found := false
			for _, text := range host.Addresses {
				v, err := netip.ParseAddr(text)
				if err == nil && v.Unmap() == ip {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		matches = append(matches, host.Node)
	}
	switch len(matches) {
	case 0:
		return status.Node{}, status.Error("HOST_NOT_FOUND", "no matching host")
	case 1:
		return matches[0], nil
	default:
		return status.Node{}, &status.Failure{Code: "AMBIGUOUS_HOST", Message: "use node_id to choose a host", Candidates: matches}
	}
}
