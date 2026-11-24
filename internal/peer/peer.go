package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/service"
	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	wire "github.com/TakashiSasaki/host-status-mcp/proto/hoststatus/peer/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
)

const (
	requestTimeout    = 5 * time.Second
	heartbeatInterval = 30 * time.Second
	silenceTimeout    = 90 * time.Second
	handshakeTimeout  = 3 * time.Second
	maxFrame          = 1 << 20
	queueSize         = 64
	maxRequests       = 16
)

type Directory interface {
	Self() status.Node
	Nodes() []status.Node
	Merge([]status.Node) (bool, []string, error)
}

type Config struct {
	Seeds         []string
	DenyNodes     []string
	TrustedRelays []string
	Refresh       func() (bool, error)
}

type Peer struct {
	wire.UnimplementedPeeringServer
	ctx       context.Context
	cancel    context.CancelFunc
	server    *grpc.Server
	wg        sync.WaitGroup
	log       *slog.Logger
	dir       Directory
	collect   status.Collector
	config    Config
	dial      func(context.Context, string) (net.Conn, error)
	jobs      chan struct{}
	relayJobs chan struct{}

	mu      sync.RWMutex
	links   map[string]*link
	seen    map[string]time.Time
	seedIDs map[string]string
	dirty   chan struct{}
}

func Start(ctx context.Context, cfg Config, listener net.Listener, dir Directory, collect status.Collector,
	dial func(context.Context, string) (net.Conn, error), logger *slog.Logger) *Peer {
	ctx, cancel := context.WithCancel(ctx)
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}
	}
	if logger == nil {
		logger = slog.Default()
	}
	p := &Peer{
		ctx: ctx, cancel: cancel, dir: dir, collect: collect, config: cfg, dial: dial, log: logger,
		jobs: make(chan struct{}, maxRequests), relayJobs: make(chan struct{}, maxRequests), links: make(map[string]*link),
		seen: make(map[string]time.Time), seedIDs: make(map[string]string), dirty: make(chan struct{}, 1),
	}
	if listener != nil {
		p.server = grpc.NewServer(grpc.MaxRecvMsgSize(maxFrame), grpc.MaxSendMsgSize(maxFrame), grpc.WaitForHandlers(true))
		wire.RegisterPeeringServer(p.server, p)
		p.wg.Go(func() {
			if err := p.server.Serve(listener); err != nil && p.ctx.Err() == nil {
				p.log.Error("peer listener stopped", "err", err)
				p.cancel()
			}
		})
	}
	p.wg.Go(p.manage)
	return p
}

func (p *Peer) Done() <-chan struct{} { return p.ctx.Done() }

func (p *Peer) Close() {
	p.cancel()
	if p.server != nil {
		p.server.Stop()
	}
	p.wg.Wait()
}

func (p *Peer) denied(id string) bool { return slices.Contains(p.config.DenyNodes, id) }

func (p *Peer) List() []status.Host {
	nodes := p.dir.Nodes()
	self := p.dir.Self().ID
	out := make([]status.Host, 0, len(nodes))
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, n := range nodes {
		if p.denied(n.ID) {
			continue
		}
		h := status.Host{Node: n, Connection: "unknown"}
		if seen, ok := p.seen[n.ID]; ok {
			h.LastSeenAt = &seen
			h.Connection = "disconnected"
		}
		if n.ID == self || live(p.links[n.ID]) || p.relayForLocked(n.ID) != nil {
			h.Connection = "connected"
		}
		if n.ID == self {
			now := time.Now().UTC()
			h.LastSeenAt = &now
		}
		out = append(out, h)
	}
	return out
}

func (p *Peer) Get(ctx context.Context, q status.Query) (result status.Result, err error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var target string
	var sections []string
	defer func() { p.audit("local", target, sections, result, err, time.Since(start)) }()
	sections, err = status.ValidateSections(q.Sections)
	if err != nil {
		return status.Result{}, err
	}
	self := p.dir.Self()
	n, err := service.Resolve(self, p.List(), q.Target)
	if err != nil {
		return status.Result{}, err
	}
	target = n.ID
	if n.ID == self.ID {
		return p.collectLocal(ctx, sections)
	}
	p.mu.RLock()
	direct := p.links[n.ID]
	p.mu.RUnlock()
	if live(direct) {
		r, err := direct.query(ctx, n.ID, sections, "")
		if err == nil {
			r.Route = status.Route{Kind: "direct"}
			return r, nil
		}
		if failure(err).Code != "UNREACHABLE" || ctx.Err() != nil {
			return status.Result{}, err
		}
	}
	p.mu.RLock()
	relay := p.relayForLocked(n.ID)
	p.mu.RUnlock()
	if relay == nil {
		return status.Result{}, status.Error("UNREACHABLE", "no live route to host")
	}
	r, err := relay.query(ctx, n.ID, sections, "")
	if err == nil {
		r.Route = status.Route{Kind: "relay", RelayID: relay.remote}
	}
	return r, err
}

func (p *Peer) audit(source, target string, sections []string, r status.Result, err error, elapsed time.Duration) {
	code := "OK"
	if err != nil {
		code = failure(err).Code
	}
	p.log.Info("status query", "source", source, "target", target, "sections", sections, "route", r.Route.Kind, "duration_ms", elapsed.Milliseconds(), "result", code)
}

func (p *Peer) collectLocal(ctx context.Context, sections []string) (status.Result, error) {
	select {
	case p.jobs <- struct{}{}:
		defer func() { <-p.jobs }()
	default:
		return status.Result{}, status.Error("BUSY", "status collection is saturated")
	}
	return p.collect(ctx, p.dir.Self(), sections)
}

func failure(err error) *status.Failure {
	var f *status.Failure
	switch {
	case errors.As(err, &f):
		return f
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error("DEADLINE_EXCEEDED", "status request timed out")
	case errors.Is(err, context.Canceled):
		return status.Error("CANCELED", "status request was canceled")
	default:
		return status.Error("COLLECTION_FAILED", "status collection failed")
	}
}

func ValidEndpoint(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !ip.IsUnspecified() && !ip.IsMulticast()
	}
	if len(host) > 253 {
		return false
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func validID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.Version() == 4 && u.String() == id
}

func nodeToWire(n status.Node) *wire.NodeRecord {
	return &wire.NodeRecord{Id: n.ID, Hostname: n.Hostname, Os: n.OS, Addresses: n.Addresses, Endpoints: n.Endpoints, Revision: n.Revision, Relay: n.Relay}
}

func nodeFromWire(r *wire.NodeRecord) (status.Node, error) {
	if r == nil || !validID(r.Id) || r.Revision == 0 || len(r.Hostname) == 0 || len(r.Hostname) > 255 ||
		strings.ContainsAny(r.Hostname, "\x00\r\n") || len(r.Os) == 0 || len(r.Os) > 32 || len(r.Addresses) > 64 || len(r.Endpoints) > 16 {
		return status.Node{}, fmt.Errorf("invalid node record")
	}
	ips := make([]string, 0, len(r.Addresses))
	for _, addr := range r.Addresses {
		ip, err := netip.ParseAddr(addr)
		if err != nil || ip.Zone() != "" {
			return status.Node{}, fmt.Errorf("invalid node IP")
		}
		ips = append(ips, ip.Unmap().String())
	}
	endpoints := slices.Clone(r.Endpoints)
	for _, addr := range endpoints {
		if !ValidEndpoint(addr) {
			return status.Node{}, fmt.Errorf("invalid peer endpoint")
		}
	}
	slices.Sort(ips)
	slices.Sort(endpoints)
	return status.Node{ID: r.Id, Hostname: r.Hostname, OS: r.Os, Addresses: slices.Compact(ips), Endpoints: slices.Compact(endpoints), Revision: r.Revision, Relay: r.Relay}, nil
}
