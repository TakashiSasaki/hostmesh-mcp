package peer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
	wire "github.com/TakashiSasaki/hostmesh-mcp/proto/hoststatus/peer/v1"
)

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var f *status.Failure
	if !errors.As(err, &f) || f.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func knownRelay(p *Peer, target string) bool {
	for _, host := range p.List() {
		if host.ID == target && host.Connection == "connected" {
			p.mu.RLock()
			defer p.mu.RUnlock()
			return p.relayForLocked(target) != nil && !live(p.links[target])
		}
	}
	return false
}

func TestRelayAndRecovery(t *testing.T) {
	aL, bL, cL := testListener(t), testListener(t), testListener(t)
	aDir := testDirectory(t, "a", []string{aL.Addr().String()}, false)
	bDir := testDirectory(t, "b", []string{bL.Addr().String()}, false)
	cDir := testDirectory(t, "c", []string{cL.Addr().String()}, true)
	aID, bID, cID := aDir.Self().ID, bDir.Self().ID, cDir.Self().ID
	var blockDirect atomic.Bool
	blockDirect.Store(true)
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		if addr != cL.Addr().String() && blockDirect.Load() {
			return nil, errors.New("direct test link disabled")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	noDial := func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("relay receives incoming links only")
	}
	c := startTest(t, Config{}, cL, cDir, 333, noDial)
	cfg := Config{Seeds: []string{cL.Addr().String()}, TrustedRelays: []string{cID}}
	a := startTest(t, cfg, aL, aDir, 111, dial)
	b := startTest(t, cfg, bL, bDir, 222, dial)
	q := status.Query{Target: &status.Target{ID: bID}, Sections: []string{"memory"}}
	waitFor(t, 10*time.Second, "first relay route", func() bool {
		return knownRelay(a, bID)
	})
	got := mcpRead(t, a, bID)
	if got.Route.Kind != "relay" || got.Route.RelayID != cID || got.Node.ID != bID || got.Sections.Memory == nil || got.Sections.Memory.TotalBytes != 222 {
		t.Fatalf("wrong relay result: %+v", got)
	}
	if linkInitiator(c, aID) != aID || linkInitiator(c, bID) != bID {
		t.Fatal("expected both nodes to initiate their link to C")
	}

	blockDirect.Store(false)
	waitFor(t, 15*time.Second, "direct recovery", func() bool { return hasSharedLink(a, b) })
	if r := mcpRead(t, a, bID); r.Route.Kind != "direct" || r.Sections.Memory.TotalBytes != 222 {
		t.Fatalf("direct not preferred: %+v", r)
	}

	blockDirect.Store(true)
	a.mu.RLock()
	l := a.links[bID]
	a.mu.RUnlock()
	l.stop()
	waitFor(t, 5*time.Second, "direct disconnect", func() bool { return activeLinkID(a, bID) == "" })
	if r := mcpRead(t, a, bID); r.Route.Kind != "relay" {
		t.Fatalf("relay not used after disconnect: %+v", r)
	}

	c.Close()
	waitFor(t, 5*time.Second, "relay disconnect", func() bool { return activeLinkID(a, cID) == "" })
	_, err := a.Get(t.Context(), q)
	expectCode(t, err, "UNREACHABLE")
	for _, h := range a.List() {
		if h.ID == bID {
			if h.Connection != "disconnected" || h.LastSeenAt == nil {
				t.Fatalf("incorrect offline host: %+v", h)
			}
			return
		}
	}
	t.Fatal("disconnected host disappeared from directory")
}

func TestRelayCancellationAndAuthorization(t *testing.T) {
	l := testListener(t)
	cDir := testDirectory(t, "c", nil, true)
	aDir, bDir := testDirectory(t, "a", nil, false), testDirectory(t, "b", nil, false)
	aID, bID, cID := aDir.Self().ID, bDir.Self().ID, cDir.Self().ID
	c := startTest(t, Config{}, l, cDir, 333, nil)

	in := make(chan struct{})
	stopped := make(chan error, 1)
	var slow atomic.Bool
	slow.Store(true)
	cfg := Config{Seeds: []string{l.Addr().String()}, TrustedRelays: []string{cID}}
	b := Start(t.Context(), cfg, nil, bDir, func(ctx context.Context, n status.Node, s []string) (status.Result, error) {
		if slow.CompareAndSwap(true, false) {
			close(in)
			<-ctx.Done()
			stopped <- ctx.Err()
			return status.Result{}, ctx.Err()
		}
		return fixture(222)(ctx, n, s)
	}, nil, quietLogger)
	t.Cleanup(b.Close)
	a := startTest(t, cfg, nil, aDir, 111, nil)
	waitFor(t, 10*time.Second, "relay", func() bool {
		return knownRelay(a, bID)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	q := status.Query{Target: &status.Target{ID: bID}, Sections: []string{"memory"}}
	done := make(chan error, 1)
	go func() { _, err := a.Get(ctx, q); done <- err }()
	select {
	case <-in:
	case err := <-done:
		t.Fatalf("lookup completed before target collection: %v", err)
	case <-ctx.Done():
		t.Fatal("target did not start collection")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup after cancel = %v", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("target not canceled: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not propagate to target")
	}
	if r := mcpRead(t, a, bID); r.Sections.Memory.TotalBytes != 222 {
		t.Fatalf("unrelated reply: %+v", r)
	}

	// A plain incoming stream cannot assert that it is a relay.
	c.mu.RLock()
	lFromC := c.links[bID]
	c.mu.RUnlock()
	withDeadline, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	_, err := lFromC.query(withDeadline, aID, []string{"memory"}, aID)
	expectCode(t, err, "FORBIDDEN") // B must not forward a forwarded request.
	_, err = b.serveRequest(withDeadline, aID, forwardedRequest(bID, cID))
	expectCode(t, err, "FORBIDDEN")
}

func TestDeadline(t *testing.T) {
	l := testListener(t)
	aDir, bDir := testDirectory(t, "a", nil, false), testDirectory(t, "b", nil, false)
	b := Start(t.Context(), Config{}, l, bDir, func(ctx context.Context, _ status.Node, _ []string) (status.Result, error) {
		<-ctx.Done()
		return status.Result{}, ctx.Err()
	}, nil, quietLogger)
	t.Cleanup(b.Close)
	a := startTest(t, Config{Seeds: []string{l.Addr().String()}}, nil, aDir, 111, nil)
	waitFor(t, 5*time.Second, "direct link", func() bool { return hasSharedLink(a, b) })
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := a.Get(ctx, status.Query{Target: &status.Target{ID: bDir.Self().ID}, Sections: []string{"memory"}})
	if failure(err).Code != "DEADLINE_EXCEEDED" {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func forwardedRequest(target, origin string) *wire.StatusRequest {
	return &wire.StatusRequest{TargetId: target, OriginId: origin, Sections: []string{"memory"}}
}

func TestLinkLostDuringLookup(t *testing.T) {
	l := testListener(t)
	aDir, bDir := testDirectory(t, "a", nil, false), testDirectory(t, "b", nil, false)
	started := make(chan struct{})
	b := Start(t.Context(), Config{}, l, bDir, func(ctx context.Context, _ status.Node, _ []string) (status.Result, error) {
		close(started)
		<-ctx.Done()
		return status.Result{}, ctx.Err()
	}, nil, quietLogger)
	t.Cleanup(b.Close)
	a := startTest(t, Config{Seeds: []string{l.Addr().String()}}, nil, aDir, 111, nil)
	waitFor(t, 5*time.Second, "live link", func() bool { return hasSharedLink(a, b) })
	done := make(chan error, 1)
	go func() {
		_, err := a.Get(t.Context(), status.Query{Target: &status.Target{ID: bDir.Self().ID}, Sections: []string{"memory"}})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not start")
	}
	b.Close()
	select {
	case err := <-done:
		expectCode(t, err, "UNREACHABLE")
	case <-time.After(time.Second):
		t.Fatal(fmt.Errorf("request stayed pending"))
	}
}

func TestBusyForSaturatedCollector(t *testing.T) {
	l := testListener(t)
	aDir, bDir := testDirectory(t, "a", nil, false), testDirectory(t, "b", nil, false)
	b := startTest(t, Config{}, l, bDir, 222, nil)
	a := startTest(t, Config{Seeds: []string{l.Addr().String()}}, nil, aDir, 111, nil)
	waitFor(t, 5*time.Second, "direct link", func() bool { return hasSharedLink(a, b) })
	for range cap(b.jobs) {
		b.jobs <- struct{}{}
	}
	q := status.Query{Target: &status.Target{ID: bDir.Self().ID}, Sections: []string{"memory"}}
	_, err := a.Get(t.Context(), q)
	expectCode(t, err, "BUSY")
	for range cap(b.jobs) {
		<-b.jobs
	}
	got, err := a.Get(t.Context(), q)
	if err != nil || got.Sections.Memory == nil || got.Sections.Memory.TotalBytes != 222 {
		t.Fatalf("collector did not recover: %+v, %v", got, err)
	}
}

func TestPeerHidesInterfaceAddresses(t *testing.T) {
	dir := testDirectory(t, "hidden", nil, false)
	local := []status.Interface{{Name: "lan0", Up: true, Addresses: []string{"192.0.2.32/24"}}}
	collect := func(ctx context.Context, node status.Node, sections []string) (status.Result, error) {
		result, err := fixture(100)(ctx, node, sections)
		result.Sections.Network = local
		return result, err
	}
	p := Start(t.Context(), Config{HideInterfaceAddresses: true}, nil, dir, collect, nil, quietLogger)
	t.Cleanup(p.Close)
	request := &wire.StatusRequest{TargetId: dir.Self().ID, Sections: []string{"network"}}
	got, err := p.serveRequest(t.Context(), "remote-peer", request)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Node.Addresses) != 0 || len(got.Sections.Network) != 1 ||
		len(got.Sections.Network[0].Addresses) != 0 {
		t.Fatalf("peer lookup exposed interface addresses: %+v", got)
	}
	if len(local[0].Addresses) != 1 {
		t.Fatal("peer redaction modified the collector result")
	}
}
