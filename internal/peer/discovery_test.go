package peer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
	"google.golang.org/grpc/test/bufconn"
)

func hasSharedLink(a, b *Peer) bool {
	x := activeLinkID(a, b.dir.Self().ID)
	return x != "" && activeLinkID(b, a.dir.Self().ID) == x
}

func activeLinkID(p *Peer, id string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if l := p.links[id]; l != nil && l.ctx.Err() == nil {
		return l.id
	}
	return ""
}

func TestSeedIndependence(t *testing.T) {
	for _, seed := range []int{0, 1} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var listeners [3]net.Listener
			var dirs [3]*memoryDirectory
			for i := range listeners {
				listeners[i] = testListener(t)
				dirs[i] = newMemoryDirectory(i, listeners[i].Addr().String(), false)
			}
			// A and B intentionally share their display name and IP.
			for i := range 2 {
				n := dirs[i].Self()
				n.Hostname = "build"
				dirs[i].nodes[n.ID] = n
			}
			a := startTest(t, Config{}, listeners[0], dirs[0], 100, nil)
			b := startTest(t, Config{Seeds: []string{listeners[0].Addr().String()}}, listeners[1], dirs[1], 200, nil)
			waitFor(t, 10*time.Second, "initial A–B link", func() bool { return hasSharedLink(a, b) })
			d := startTest(t, Config{Seeds: []string{listeners[seed].Addr().String()}}, listeners[2], dirs[2], 300, nil)
			waitFor(t, 10*time.Second, "three-node mesh", func() bool {
				return len(a.List()) == 3 && len(b.List()) == 3 && len(d.List()) == 3 &&
					hasSharedLink(a, b) && hasSharedLink(a, d) && hasSharedLink(b, d)
			})
			_, err := d.Get(t.Context(), status.Query{Target: &status.Target{Hostname: "build", IP: "192.0.2.32"}, Sections: []string{"memory"}})
			var f *status.Failure
			if !errors.As(err, &f) || f.Code != "AMBIGUOUS_HOST" || len(f.Candidates) != 2 {
				t.Fatalf("ambiguous lookup = %v", err)
			}
			got := mcpRead(t, d, dirs[1].Self().ID)
			if got.Sections.Memory == nil || got.Sections.Memory.TotalBytes != 200 {
				t.Fatalf("wrong target: %+v", got)
			}
		})
	}
}

func TestUpdateAfterPartition(t *testing.T) {
	aListener := testListener(t)
	aDir, bDir := testDirectory(t, "a", nil, false), testDirectory(t, "old", nil, false)
	a := startTest(t, Config{}, aListener, aDir, 100, nil)
	cfg := Config{Seeds: []string{aListener.Addr().String()}}
	b := startTest(t, cfg, nil, bDir, 200, nil)
	id := bDir.Self().ID
	waitFor(t, 5*time.Second, "first direct link", func() bool { return hasSharedLink(a, b) })
	b.Close()
	waitFor(t, 5*time.Second, "disconnect", func() bool { return activeLinkID(a, id) == "" })
	record := bDir.Self()
	record.Hostname, record.Addresses = "new", []string{"192.0.2.33"}
	if changed, err := bDir.UpdateSelf(record); !changed || err != nil {
		t.Fatalf("update self = %v: %v", changed, err)
	}
	b = startTest(t, cfg, nil, bDir, 201, nil)
	waitFor(t, 5*time.Second, "new record after reconnect", func() bool {
		for _, h := range a.List() {
			if h.ID == id {
				return h.Hostname == "new" && slices.Equal(h.Addresses, record.Addresses) && h.Revision > record.Revision
			}
		}
		return false
	})
	if got := mcpRead(t, a, id); got.Sections.Memory == nil || got.Sections.Memory.TotalBytes != 201 {
		t.Fatalf("unexpected result %+v", got)
	}
}

type memoryDirectory struct {
	id    string
	mu    sync.RWMutex
	nodes map[string]status.Node
}

func newMemoryDirectory(i int, endpoint string, relay bool) *memoryDirectory {
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
	n := status.Node{ID: id, Hostname: fmt.Sprintf("node-%d", i), OS: "linux", Revision: 1, Addresses: []string{"192.0.2.32"}, Relay: relay}
	if endpoint != "" {
		n.Endpoints = []string{endpoint}
	}
	return &memoryDirectory{id: id, nodes: map[string]status.Node{id: n}}
}

func (d *memoryDirectory) Self() status.Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.nodes[d.id]
}

func (d *memoryDirectory) Nodes() []status.Node {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return slices.Collect(maps.Values(d.nodes))
}

func (d *memoryDirectory) Merge(records []status.Node) (bool, []string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, n := range records {
		if n.ID != d.id && n.Revision > d.nodes[n.ID].Revision {
			d.nodes[n.ID] = n
			changed = true
		}
	}
	return changed, nil, nil
}

func TestMesh100(t *testing.T) {
	if os.Getenv("HOSTMESH_MCP_SCALE_TEST") != "1" {
		t.Skip("set HOSTMESH_MCP_SCALE_TEST=1 for the 100-node test")
	}
	t.Run("full-mesh", func(t *testing.T) { runScale(t, false) })
	t.Run("single-relay", func(t *testing.T) { runScale(t, true) })
}

func runScale(t *testing.T, star bool) {
	const count = 100
	listeners := make(map[string]*bufconn.Listener)
	endpoints := make([]string, count)
	for i := range count {
		if star && i > 0 {
			break
		}
		endpoints[i] = fmt.Sprintf("127.0.0.1:%d", 15000+i)
		listener := bufconn.Listen(8192)
		listeners[endpoints[i]] = listener
		t.Cleanup(func() { listener.Close() })
	}
	dial := func(ctx context.Context, address string) (net.Conn, error) {
		if l := listeners[address]; l != nil {
			return l.DialContext(ctx)
		}
		return nil, fmt.Errorf("unknown test endpoint %s", address)
	}
	started := time.Now()
	peers := make([]*Peer, count)
	for i := range count {
		d := newMemoryDirectory(i, endpoints[i], star && i == 0)
		cfg := Config{}
		if i != 0 {
			cfg.Seeds = []string{endpoints[0]}
			if star {
				cfg.TrustedRelays = []string{peers[0].dir.Self().ID}
			}
		}
		var listener net.Listener
		if endpoints[i] != "" {
			listener = listeners[endpoints[i]]
		}
		peers[i] = startTest(t, cfg, listener, d, uint64(i+1), dial)
	}
	waitFor(t, 90*time.Second, "100-node directory and topology", func() bool {
		for i, p := range peers {
			if len(p.dir.Nodes()) != count {
				return false
			}
			if star && i > 0 && !hasSharedLink(p, peers[0]) {
				return false
			}
			if !star {
				for _, q := range peers[:i] {
					if !hasSharedLink(p, q) {
						return false
					}
				}
			}
		}
		return true
	})
	target := peers[51].dir.Self().ID
	source := peers[count-1]
	route, expectedLinks := "direct", count*(count-1)/2
	if star {
		route, expectedLinks = "relay", count-1
		waitFor(t, 5*time.Second, "scale relay reachability", func() bool {
			source.mu.RLock()
			defer source.mu.RUnlock()
			return source.relayForLocked(target) != nil
		})
	}
	sum := 0
	for _, p := range peers {
		p.mu.RLock()
		for _, link := range p.links {
			if live(link) {
				sum++
			}
		}
		p.mu.RUnlock()
	}
	if sum != 2*expectedLinks {
		t.Fatalf("connected ends = %d, expected %d", sum, 2*expectedLinks)
	}
	if r := mcpRead(t, source, target); r.Route.Kind != route || r.Sections.Memory == nil || r.Sections.Memory.TotalBytes != 52 {
		t.Fatalf("bad scale result %+v", r)
	}
	t.Logf("%d nodes and %d links converged in %s", count, expectedLinks, time.Since(started))
	samples := make([]time.Duration, count)
	var queries sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, src := range peers {
		slots <- struct{}{}
		queries.Go(func() {
			defer func() { <-slots }()
			j := (i + 1) % count
			q := status.Query{Target: &status.Target{ID: peers[j].dir.Self().ID}, Sections: []string{"memory"}}
			begin := time.Now()
			r, err := src.Get(t.Context(), q)
			samples[i] = time.Since(begin)
			wantRoute := "direct"
			if star && i != 0 && j != 0 {
				wantRoute = "relay"
			}
			if err != nil || r.Node.ID != q.Target.ID || r.Route.Kind != wantRoute || r.Sections.Memory == nil || r.Sections.Memory.TotalBytes != uint64(j+1) {
				t.Errorf("scale lookup %d -> %d: %+v, %v", i, j, r, err)
			}
		})
	}
	queries.Wait()
	slices.Sort(samples)
	t.Logf("100 lookups, concurrency 8: p50=%s p95=%s p99=%s", samples[49], samples[94], samples[98])
}
