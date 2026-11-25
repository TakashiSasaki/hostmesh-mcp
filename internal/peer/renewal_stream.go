package peer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"time"

	wire "github.com/TakashiSasaki/hostmesh-mcp/proto/hoststatus/peer/v1"
	"github.com/google/uuid"
)

const (
	maxRenewalHops   = 8
	renewalTimeout   = 30 * time.Second
	renewalChunk     = 8192
	maxExchangeBytes = 512 << 10
)

func validRenewalPath(path []string) bool {
	if len(path) > maxRenewalHops {
		return false
	}
	for i, id := range path {
		if !validID(id) || slices.Contains(path[:i], id) {
			return false
		}
	}
	return true
}

// Path vectors keep advertisements loop-free. A gateway only dials its local
// configuration; no address supplied in a frame ever reaches the TCP dialer.
func (l *link) updateRenewalPath(path []string) bool {
	p := l.owner
	if len(path) > 0 && (p.config.TLS == nil || path[0] != l.remote || !validRenewalPath(path)) {
		return false
	}
	if p.config.TLS == nil || p.config.Renewal.URL == "" && !p.config.Renewal.Relay {
		return true
	}
	if slices.Contains(path, p.config.TLS.NodeID()) || slices.ContainsFunc(path, p.denied) {
		path = nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(path, l.renewalPath) {
		l.renewalPath = slices.Clone(path)
		p.markDirty()
	}
	return true
}

func (p *Peer) renewalPathLocked() []string {
	if p.config.Renewal.Gateway != "" {
		return nil
	}
	var best []string
	for _, l := range p.links {
		path := l.renewalPath
		if live(l) && len(path) > 0 && (best == nil || len(path) < len(best) || len(path) == len(best) && path[0] < best[0]) {
			best = path
		}
	}
	return best
}

func (p *Peer) dialRenewal(ctx context.Context, path []string) (net.Conn, error) {
	if len(path) == 0 {
		if p.config.Renewal.Gateway == "" {
			return nil, errors.New("no live route to renewal CA")
		}
		return p.dial(ctx, p.config.Renewal.Gateway)
	}
	p.mu.RLock()
	l := p.links[path[0]]
	ok := live(l) && slices.Equal(l.renewalPath, path)
	p.mu.RUnlock()
	if !ok {
		return nil, errors.New("renewal route changed")
	}
	id := uuid.NewString()
	conn, err := l.newRenewalPipe(id)
	if err != nil {
		return nil, err
	}
	if !l.sendRenewal(&wire.RenewalStream{Id: id, Action: &wire.RenewalStream_Open{Open: &wire.RenewalOpen{Path: path}}}) {
		conn.Close()
		return nil, net.ErrClosed
	}
	return conn, nil
}

type renewalPipe struct {
	input chan []byte // nil is an ordered end-of-stream marker
	stop  context.CancelFunc
}

func (l *link) sendRenewal(m *wire.RenewalStream) bool {
	// A saturated renewal must not disconnect an otherwise healthy status link.
	select {
	case <-l.ctx.Done():
		return false
	case l.tx <- outgoingFrame{frame: &wire.Frame{Body: &wire.Frame_Renewal{Renewal: m}}}:
		return true
	default:
		return false
	}
}

func (l *link) closeRenewal(id string) {
	l.sendRenewal(&wire.RenewalStream{Id: id, Action: &wire.RenewalStream_Close{Close: true}})
}

// Present a net.Conn to TLS and forward ordered chunks independently of the
// link reader. Time, bytes, queue depth and total active pipes are bounded.
func (l *link) newRenewalPipe(id string) (net.Conn, error) {
	p := l.owner
	select {
	case p.renewalJobs <- struct{}{}:
	default:
		return nil, errors.New("renewal window full")
	}
	ctx, stop := context.WithTimeout(l.ctx, renewalTimeout)
	pipe := &renewalPipe{input: make(chan []byte, 16), stop: stop}
	l.mu.Lock()
	if l.renewals[id] != nil {
		l.mu.Unlock()
		stop()
		<-p.renewalJobs
		return nil, errors.New("duplicate renewal stream")
	}
	l.renewals[id] = pipe
	l.mu.Unlock()
	app, transport := net.Pipe()
	closeConn := context.AfterFunc(ctx, func() { transport.Close() })
	p.wg.Go(func() {
		defer func() {
			stop()
			closeConn()
			transport.Close()
			l.mu.Lock()
			delete(l.renewals, id)
			l.mu.Unlock()
			<-p.renewalJobs
			l.closeRenewal(id)
		}()
		buf := make([]byte, renewalChunk)
		total := 0
		for {
			n, err := transport.Read(buf)
			if n > 0 {
				total += n
				if total > maxExchangeBytes || !l.sendRenewal(&wire.RenewalStream{Id: id, Action: &wire.RenewalStream_Data{Data: bytes.Clone(buf[:n])}}) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	p.wg.Go(func() {
		defer stop()
		total := 0
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-pipe.input:
				total += len(data)
				if len(data) == 0 || total > maxExchangeBytes {
					return
				}
				if _, err := transport.Write(data); err != nil {
					return
				}
			}
		}
	})
	return app, nil
}

func (l *link) receiveRenewal(m *wire.RenewalStream) bool {
	p := l.owner
	if p.config.TLS == nil || m == nil || !validID(m.Id) {
		return false
	}
	var data []byte
	switch v := m.Action.(type) {
	case *wire.RenewalStream_Open:
		if v.Open == nil || len(v.Open.Path) == 0 || !validRenewalPath(v.Open.Path) {
			return false
		}
		path := v.Open.Path
		if path[0] != p.config.TLS.NodeID() || slices.Contains(path, l.remote) || slices.ContainsFunc(path, p.denied) ||
			len(path) == 1 && p.config.Renewal.Gateway == "" || len(path) > 1 && !p.config.Renewal.Relay {
			l.closeRenewal(m.Id)
			return true
		}
		upstream, err := l.newRenewalPipe(m.Id)
		if err != nil {
			l.closeRenewal(m.Id)
			return true
		}
		p.wg.Go(func() {
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(l.ctx, renewalTimeout)
			defer cancel()
			dst, err := p.dialRenewal(ctx, path[1:])
			if err != nil {
				return
			}
			defer dst.Close()
			dst.SetDeadline(time.Now().Add(renewalTimeout))
			// The TLS client sends one HTTPS POST; the server returns one response.
			// Closing both endpoints wakes the other copier on timeout or link loss.
			done := make(chan struct{})
			go func() { defer close(done); io.Copy(dst, upstream); dst.Close() }()
			io.Copy(upstream, dst)
			upstream.Close()
			dst.Close()
			<-done
		})
		return true
	case *wire.RenewalStream_Data:
		if len(v.Data) == 0 || len(v.Data) > renewalChunk {
			return false
		}
		data = v.Data
	case *wire.RenewalStream_Close:
	default:
		return false
	}
	l.mu.Lock()
	pipe := l.renewals[m.Id]
	l.mu.Unlock()
	if pipe != nil {
		select {
		case pipe.input <- data:
		default:
			pipe.stop()
		}
	}
	return true
}
