package peer

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	wire "github.com/TakashiSasaki/host-status-mcp/proto/hoststatus/peer/v1"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type stream interface {
	Context() context.Context
	Send(*wire.Frame) error
	Recv() (*wire.Frame, error)
}

type outgoingFrame struct {
	frame    *wire.Frame
	deadline time.Time
}

type link struct {
	owner     *Peer
	ctx       context.Context
	stop      context.CancelFunc
	id        string
	initiator string
	remote    string
	tx        chan outgoingFrame

	mu       sync.Mutex
	pending  map[string]chan *wire.StatusResponse
	incoming map[string]context.CancelFunc
}

func (p *Peer) Link(s grpc.BidiStreamingServer[wire.Frame, wire.Frame]) error {
	p.runStream(s, "", "", "", nil)
	return nil
}

func (p *Peer) seedLoop(endpoint string) {
	delay := time.Second
	for p.ctx.Err() == nil {
		p.mu.RLock()
		l := p.links[p.seedIDs[endpoint]]
		p.mu.RUnlock()
		if l != nil {
			select {
			case <-l.ctx.Done():
			case <-p.ctx.Done():
				return
			}
		}
		if p.connect(endpoint, "", endpoint) {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-p.ctx.Done():
			timer.Stop()
			return
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (p *Peer) connect(endpoint, expectedID, seed string) bool {
	c, err := grpc.NewClient("passthrough:///"+endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(p.dial),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxFrame), grpc.MaxCallSendMsgSize(maxFrame)))
	if err != nil {
		return false
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	timer := time.AfterFunc(handshakeTimeout, cancel)
	defer timer.Stop()
	s, err := wire.NewPeeringClient(c).Link(ctx)
	if err != nil {
		return false
	}
	return p.runStream(s, uuid.NewString(), expectedID, seed, func() { timer.Stop() })
}

func hello(self status.Node, id string) *wire.Frame {
	return &wire.Frame{Body: &wire.Frame_Hello{Hello: &wire.Hello{Version: 1, LinkId: id, Node: nodeToWire(self)}}}
}

func (p *Peer) runStream(s stream, id, expectedID, seed string, established func()) bool {
	ctx, stop := context.WithCancel(s.Context())
	unsubscribe := context.AfterFunc(p.ctx, stop)
	defer unsubscribe()
	defer stop()
	l := &link{owner: p, ctx: ctx, stop: stop, id: id, tx: make(chan outgoingFrame, queueSize),
		pending: make(map[string]chan *wire.StatusResponse), incoming: make(map[string]context.CancelFunc)}
	rx := make(chan *wire.Frame)
	go func() {
		defer stop()
		for {
			msg, err := s.Recv()
			if err != nil {
				return
			}
			select {
			case rx <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	go l.write(s)
	self := p.dir.Self()
	if id != "" {
		l.initiator = self.ID
		l.send(hello(self, id))
	}
	timer := time.NewTimer(handshakeTimeout)
	defer timer.Stop()
	var first *wire.Frame
	select {
	case first = <-rx:
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
	h := first.GetHello()
	if h == nil || h.Version != 1 || !validID(h.LinkId) || id != "" && h.LinkId != id {
		return false
	}
	n, err := nodeFromWire(h.Node)
	if err != nil || n.ID == self.ID || p.denied(n.ID) || expectedID != "" && n.ID != expectedID {
		return false
	}
	l.remote = n.ID
	if id == "" {
		l.id, l.initiator = h.LinkId, n.ID
		l.send(hello(self, l.id))
	}
	if _, _, err := p.dir.Merge([]status.Node{n}); err != nil {
		p.log.Error("save peer record", "err", err)
		return false
	}
	if established != nil {
		established()
	}
	if !p.register(l, seed) {
		return false
	}
	defer p.unregister(l)
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	expiration := time.NewTimer(silenceTimeout)
	defer expiration.Stop()
	for {
		select {
		case frame := <-rx:
			if !l.receive(frame) {
				return true
			}
			expiration.Reset(silenceTimeout)
			p.mu.Lock()
			if p.links[l.remote] == l {
				p.seen[l.remote] = time.Now().UTC()
			}
			p.mu.Unlock()
		case <-heartbeat.C:
			l.send(&wire.Frame{Body: &wire.Frame_Heartbeat{Heartbeat: &wire.Heartbeat{}}})
		case <-expiration.C:
			return true
		case <-ctx.Done():
			return true
		}
	}
}

func (p *Peer) register(l *link, seed string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if seed != "" {
		p.seedIDs[seed] = l.remote
	}
	if old := p.links[l.remote]; old != nil && old.ctx.Err() == nil {
		if old.initiator < l.initiator || old.initiator == l.initiator && old.id <= l.id {
			return false
		}
		old.stop()
	}
	p.links[l.remote] = l
	p.seen[l.remote] = time.Now().UTC()
	return true
}

func (p *Peer) unregister(l *link) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.links[l.remote] == l {
		delete(p.links, l.remote)
	}
}

func (l *link) send(f *wire.Frame) bool { return l.enqueue(outgoingFrame{frame: f}) }

func (l *link) enqueue(f outgoingFrame) bool {
	select {
	case <-l.ctx.Done():
		return false
	case l.tx <- f:
		return true
	default:
		l.stop()
		return false
	}
}

func (l *link) write(s stream) {
	defer l.stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case item := <-l.tx:
			if !item.deadline.IsZero() {
				left := time.Until(item.deadline)
				if left <= 0 {
					continue
				}
				item.frame.GetStatusRequest().TimeoutMs = uint32(max(left.Milliseconds(), 1))
			}
			if err := s.Send(item.frame); err != nil {
				return
			}
		}
	}
}

func (l *link) receive(frame *wire.Frame) bool {
	switch b := frame.Body.(type) {
	case *wire.Frame_Heartbeat:
		return true
	case *wire.Frame_StatusRequest:
		return l.startRequest(b.StatusRequest)
	case *wire.Frame_StatusResponse:
		resp := b.StatusResponse
		if resp == nil || !validID(resp.Id) {
			return false
		}
		l.mu.Lock()
		ch := l.pending[resp.Id]
		l.mu.Unlock()
		if ch != nil {
			select {
			case ch <- resp:
			default:
			}
		}
		return true
	case *wire.Frame_Cancel:
		if b.Cancel == nil || !validID(b.Cancel.Id) {
			return false
		}
		l.mu.Lock()
		cancel := l.incoming[b.Cancel.Id]
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return true
	default:
		return false
	}
}

func (l *link) query(ctx context.Context, target string, sections []string) (status.Result, error) {
	id := uuid.NewString()
	reply := make(chan *wire.StatusResponse, 1)
	l.mu.Lock()
	if len(l.pending) >= maxRequests {
		l.mu.Unlock()
		return status.Result{}, status.Error("BUSY", "peer request window is full")
	}
	l.pending[id] = reply
	l.mu.Unlock()
	defer func() { l.mu.Lock(); delete(l.pending, id); l.mu.Unlock() }()
	deadline, _ := ctx.Deadline()
	f := &wire.Frame{Body: &wire.Frame_StatusRequest{StatusRequest: &wire.StatusRequest{Id: id, TargetId: target, Sections: sections}}}
	if !l.enqueue(outgoingFrame{frame: f, deadline: deadline}) {
		return status.Result{}, status.Error("UNREACHABLE", "peer link closed")
	}
	select {
	case resp := <-reply:
		if resp.ErrorCode != "" {
			return status.Result{}, status.Error(resp.ErrorCode, resp.ErrorMessage)
		}
		var r status.Result
		if err := json.Unmarshal(resp.ResultJson, &r); err != nil || r.Node.ID != target || r.ObservedAt.IsZero() {
			return status.Result{}, status.Error("UNREACHABLE", "invalid peer result")
		}
		return r, nil
	case <-ctx.Done():
		l.send(&wire.Frame{Body: &wire.Frame_Cancel{Cancel: &wire.Cancel{Id: id}}})
		return status.Result{}, ctx.Err()
	case <-l.ctx.Done():
		return status.Result{}, status.Error("UNREACHABLE", "peer link closed")
	}
}

func (l *link) startRequest(req *wire.StatusRequest) bool {
	if req == nil || !validID(req.Id) || !validID(req.TargetId) || req.TimeoutMs == 0 {
		return false
	}
	l.mu.Lock()
	if l.incoming[req.Id] != nil {
		l.mu.Unlock()
		return false
	}
	if len(l.incoming) >= maxRequests {
		l.mu.Unlock()
		l.reply(req.Id, status.Result{}, status.Error("BUSY", "peer request window is full"))
		return true
	}
	ctx, cancel := context.WithTimeout(l.ctx, min(time.Duration(req.TimeoutMs)*time.Millisecond, requestTimeout))
	l.incoming[req.Id] = cancel
	l.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			l.mu.Lock()
			delete(l.incoming, req.Id)
			l.mu.Unlock()
		}()
		r, err := l.owner.serveRequest(ctx, l.remote, req)
		l.owner.audit(l.remote, req.TargetId, req.Sections, r, err)
		l.reply(req.Id, r, err)
	}()
	return true
}

func (p *Peer) serveRequest(ctx context.Context, source string, req *wire.StatusRequest) (status.Result, error) {
	if p.denied(source) || req.TargetId != p.dir.Self().ID {
		return status.Result{}, status.Error("FORBIDDEN", "request target is not available on this link")
	}
	sections, err := status.ValidateSections(req.Sections)
	if err != nil {
		return status.Result{}, err
	}
	return p.collectLocal(ctx, sections)
}

func (l *link) reply(id string, r status.Result, err error) {
	resp := &wire.StatusResponse{Id: id}
	if err == nil {
		resp.ResultJson, err = json.Marshal(r)
	}
	if err != nil {
		f := failure(err)
		resp.ErrorCode, resp.ErrorMessage = f.Code, f.Message
	}
	l.send(&wire.Frame{Body: &wire.Frame_StatusResponse{StatusResponse: resp}})
}
