package peer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/mcpserver"
	"github.com/TakashiSasaki/host-status-mcp/internal/state"
	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func testDirectory(t *testing.T, name string, endpoints []string, relay bool) *state.Store {
	t.Helper()
	d, err := state.Open(t.TempDir(), status.Node{Hostname: name, OS: "linux", Addresses: []string{"192.0.2.32"}, Endpoints: endpoints, Relay: relay})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func fixture(value uint64) status.Collector {
	return func(_ context.Context, n status.Node, _ []string) (status.Result, error) {
		return status.Result{Node: n, ObservedAt: time.Now().UTC(), Route: status.Route{Kind: "local"},
			Sections: status.Sections{Memory: &status.Memory{TotalBytes: value, AvailableBytes: value / 2}},
			Errors:   map[string]string{}}, nil
	}
}

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func startTest(t *testing.T, c Config, l net.Listener, d Directory, v uint64, dial func(context.Context, string) (net.Conn, error)) *Peer {
	t.Helper()
	p := Start(t.Context(), c, l, d, fixture(v), dial, quietLogger)
	t.Cleanup(p.Close)
	return p
}

func waitFor(t *testing.T, timeout time.Duration, desc string, f func() bool) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	for !f() {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", desc, ctx.Err())
		}
	}
}

func linkInitiator(p *Peer, id string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if l := p.links[id]; l != nil && l.ctx.Err() == nil {
		return l.initiator
	}
	return ""
}

func mcpRead(t *testing.T, backend *Peer, target string) status.Result {
	t.Helper()
	http := httptest.NewServer(mcpserver.HTTP(mcpserver.New("test", backend)))
	defer http.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: http.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	x, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "host_status",
		Arguments: map[string]any{"target": map[string]string{"node_id": target}, "sections": []string{"memory"}}})
	if err != nil || x.IsError {
		t.Fatalf("MCP host_status = %+v, err = %v", x, err)
	}
	b, err := json.Marshal(x.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var r status.Result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDirect(t *testing.T) {
	for _, direction := range []string{"A-to-B", "B-to-A"} {
		t.Run(direction, func(t *testing.T) {
			a, b := testDirectory(t, "a", nil, false), testDirectory(t, "b", nil, false)
			l := testListener(t)
			seed := Config{Seeds: []string{l.Addr().String()}}
			aID, bID := a.Self().ID, b.Self().ID
			var p *Peer
			initiator := aID
			if direction == "A-to-B" {
				startTest(t, Config{}, l, b, 222, nil)
				p = startTest(t, seed, nil, a, 111, nil)
			} else {
				initiator = bID
				p = startTest(t, Config{}, l, a, 111, nil)
				startTest(t, seed, nil, b, 222, nil)
			}
			waitFor(t, 5*time.Second, "direct link", func() bool { return linkInitiator(p, bID) == initiator })
			r := mcpRead(t, p, bID)
			if r.Node.ID != bID || r.Route.Kind != "direct" || r.Sections.Memory == nil || r.Sections.Memory.TotalBytes != 222 {
				t.Fatalf("unexpected remote result: %+v", r)
			}
		})
	}
}
