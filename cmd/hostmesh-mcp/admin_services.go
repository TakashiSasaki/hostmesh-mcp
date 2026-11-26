package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type managedRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	ready  chan struct{}
	err    error       // Published by closing done.
	info   adminResult // Published by closing ready.
	update chan os.Signal
}
type adminServices struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	runs   map[string]*managedRun
}

func (s *adminServices) init(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.runs = make(map[string]*managedRun)
}
func (s *adminServices) close() {
	s.mu.Lock()
	s.cancel()
	var runs []*managedRun
	for _, r := range s.runs {
		runs = append(runs, r)
	}
	s.mu.Unlock()
	for _, r := range runs {
		<-r.done
	}
}
func (r *managedRun) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *managedRun) status() string {
	select {
	case <-r.done:
		if r.err != nil {
			return "failed"
		}
		return "stopped"
	default:
	}
	select {
	case <-r.ready:
		return "running"
	default:
		return "starting"
	}
}

type runService func(context.Context, func(adminResult), <-chan os.Signal) error

func (s *adminServices) start(ctx context.Context, name string, run runService) (adminResult, error) {
	s.mu.Lock()
	if err := s.ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current := s.runs[name]
	if current != nil && !current.finished() {
		s.mu.Unlock()
		return nil, fmt.Errorf("%s already started; use service_status", name)
	}
	child, cancel := context.WithCancel(s.ctx)
	r := &managedRun{cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), update: make(chan os.Signal, 1)}
	s.runs[name] = r
	s.mu.Unlock()
	go func() {
		defer close(r.done)
		defer cancel()
		var once sync.Once
		r.err = run(child, func(info adminResult) { once.Do(func() { r.info = info; close(r.ready) }) }, r.update)
	}()
	select {
	case <-r.ready:
		return adminResult{"service": name, "status": "running", "details": r.info}, nil
	case <-r.done:
		if r.err != nil {
			return nil, r.err
		}
		return nil, errors.New("service stopped during startup")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}
func (s *adminServices) list(context.Context, noArgs) (adminResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	services := map[string]adminResult{}
	for name, r := range s.runs {
		entry := adminResult{"status": r.status()}
		if entry["status"] == "running" {
			entry["details"] = r.info
		}
		if entry["status"] == "failed" {
			entry["error"] = r.err.Error()
		}
		services[name] = entry
	}
	return adminResult{"services": services}, nil
}

type serviceName struct {
	Service string `json:"service" jsonschema:"Session-owned service name from service_status: peer, ca, or supervisor"`
}

func (s *adminServices) stop(ctx context.Context, in serviceName) (adminResult, error) {
	s.mu.Lock()
	r := s.runs[in.Service]
	s.mu.Unlock()
	if r == nil {
		return nil, errors.New("unknown session-owned service; use service_status")
	}
	r.cancel()
	select {
	case <-r.done:
		if r.err != nil {
			return nil, r.err
		}
		return adminResult{"service": in.Service, "status": "stopped"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (a *adminServer) addServices(s *mcp.Server) {
	adminTool(s, "peer_start", "Start the bound peer configuration on this host until service_stop or administration session exit. Returns the monitoring URL (loopback HTTP) and the mesh listener. Peer mTLS protects mesh traffic.", false, false, func(ctx context.Context, _ noArgs) (adminResult, error) {
		return a.services.start(ctx, "peer", func(child context.Context, ready func(adminResult), _ <-chan os.Signal) error {
			cfg, err := readConfig(a.peerPath)
			if err != nil {
				return err
			}
			sec, err := cfg.preparePeering()
			if err != nil {
				return err
			}
			return serveReady(child, a.version, cfg, sec, func(nodeID, url, listen string) {
				ready(adminResult{"node_id": nodeID, "mcp_url": url, "peer_listen": listen, "peer_security": cfg.Peer.Security, "mcp_transport": "http"})
			})
		})
	})
	adminTool(s, "ca_start", "Start the EST renewal listener for the bound CA until stopped or disconnected. Local enrollment can run while EST is stopped.", false, false, func(ctx context.Context, _ noArgs) (adminResult, error) {
		return a.services.start(ctx, "ca", func(child context.Context, ready func(adminResult), _ <-chan os.Signal) error {
			ca, err := a.loadCA()
			if err != nil {
				return err
			}
			return ca.ServeReady(child, os.Stderr, func(listen string) { ready(adminResult{"listen": listen}) })
		})
	})
	adminTool(s, "service_status", "List services launched by this administration session and their startup, running, stopped or failed state.", true, false, a.services.list)
	adminTool(s, "service_stop", "Stop one named service owned by this administration session and wait for shutdown.", false, true, a.services.stop)
}
