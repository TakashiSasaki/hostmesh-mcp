package peer

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	wire "github.com/TakashiSasaki/host-status-mcp/proto/hoststatus/peer/v1"
)

func live(l *link) bool { return l != nil && l.ctx.Err() == nil }

func (p *Peer) trustsRelay(id string) bool {
	return !p.denied(id) && slices.Contains(p.config.TrustedRelays, id)
}

// relayForLocked returns the lowest ID trusted relay with a fresh route to target.
func (p *Peer) relayForLocked(target string) *link {
	var best *link
	for _, id := range p.config.TrustedRelays {
		if id == target || !p.trustsRelay(id) {
			continue
		}
		l := p.links[id]
		if live(l) && time.Since(l.reportedAt) < silenceTimeout && l.reachable[target] &&
			(best == nil || id < best.remote) {
			best = l
		}
	}
	return best
}

func (p *Peer) broadcastReachability() {
	if !p.dir.Self().Relay {
		return
	}
	p.mu.RLock()
	links := make([]*link, 0, len(p.links))
	ids := make([]string, 0, len(p.links))
	for id, l := range p.links {
		if live(l) {
			ids = append(ids, id)
			links = append(links, l)
		}
	}
	p.mu.RUnlock()
	sort.Strings(ids)
	msg := &wire.Frame{Body: &wire.Frame_Reachability{Reachability: &wire.Reachability{ConnectedIds: ids}}}
	for _, l := range links {
		l.send(msg)
	}
}

func (l *link) receiveReachability(report *wire.Reachability) bool {
	if report == nil || len(report.ConnectedIds) > maxInventory {
		return false
	}
	p := l.owner
	if !p.trustsRelay(l.remote) {
		return true
	}
	now := time.Now()
	ids := make(map[string]bool, len(report.ConnectedIds))
	for _, id := range report.ConnectedIds {
		if !validID(id) {
			return false
		}
		if !p.denied(id) {
			ids[id] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.links[l.remote] == l {
		l.reachable, l.reportedAt = ids, now
		for id := range ids {
			p.seen[id] = now.UTC()
		}
	}
	return true
}

func (p *Peer) serveRequest(ctx context.Context, source string, req *wire.StatusRequest) (status.Result, error) {
	if p.denied(source) || p.denied(req.TargetId) {
		return status.Result{}, status.Error("FORBIDDEN", "request is not permitted")
	}
	sections, err := status.ValidateSections(req.Sections)
	if err != nil {
		return status.Result{}, err
	}
	self := p.dir.Self()
	if req.OriginId != "" && (req.TargetId != self.ID || !p.trustsRelay(source) || p.denied(req.OriginId)) {
		return status.Result{}, status.Error("FORBIDDEN", "forwarded request is not permitted")
	}
	if req.TargetId == self.ID {
		result, err := p.collectLocal(ctx, sections)
		if err == nil && p.config.HideInterfaceAddresses {
			// Keep shared collector results independent of the peer's address policy.
			result.Node.Addresses = []string{}
			result.Sections.Network = slices.Clone(result.Sections.Network)
			for i := range result.Sections.Network {
				result.Sections.Network[i].Addresses = []string{}
			}
		}
		return result, err
	}
	if !self.Relay {
		return status.Result{}, status.Error("FORBIDDEN", "relay is disabled")
	}
	p.mu.RLock()
	target := p.links[req.TargetId]
	p.mu.RUnlock()
	if !live(target) {
		return status.Result{}, status.Error("UNREACHABLE", "relay has no direct link to target")
	}
	select {
	case p.relayJobs <- struct{}{}:
		defer func() { <-p.relayJobs }()
	default:
		return status.Result{}, status.Error("BUSY", "relay request window is full")
	}
	r, err := target.query(ctx, req.TargetId, sections, source)
	if err == nil {
		r.Route = status.Route{Kind: "relay", RelayID: self.ID}
	}
	return r, err
}
