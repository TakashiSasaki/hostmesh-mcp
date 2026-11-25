package peer

import (
	"bytes"
	"crypto/sha256"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	wire "github.com/TakashiSasaki/host-status-mcp/proto/hoststatus/peer/v1"
	"google.golang.org/protobuf/proto"
)

const (
	dialInterval       = 100 * time.Millisecond
	reconcileInterval  = 30 * time.Second
	maxConcurrentDials = 8
	maxInventory       = 512
	maxRecordBatch     = 128
)

type dialPlan struct {
	key       string
	id        string
	seed      string
	endpoints []string
}

type attempt struct {
	pending bool
	delay   time.Duration
	next    time.Time
}

type dialResult struct {
	key string
	ok  bool
}

func (p *Peer) manage() {
	tick := time.NewTicker(dialInterval)
	defer tick.Stop()
	reconcile := time.NewTicker(reconcileInterval)
	defer reconcile.Stop()
	dials := make(map[string]attempt)
	results := make(chan dialResult, maxConcurrentDials)
	active := 0
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-tick.C:
			select {
			case <-p.dirty:
				p.broadcastState()
			default:
			}
			if active < maxConcurrentDials {
				active += p.schedule(dials, results, maxConcurrentDials-active)
			}
		case r := <-results:
			active--
			a := dials[r.key]
			if r.ok {
				a = attempt{}
			} else {
				a.pending = false
				a.delay = min(max(2*a.delay, time.Second), 30*time.Second)
				a.next = time.Now().Add(a.delay/2 + rand.N(a.delay/2))
			}
			dials[r.key] = a
		case <-reconcile.C:
			if p.config.Refresh != nil {
				if _, err := p.config.Refresh(); err != nil {
					p.log.Error("refresh local node", "err", err)
				}
			}
			p.broadcastState()
		}
	}
}

func (p *Peer) schedule(attempts map[string]attempt, results chan<- dialResult, limit int) int {
	plans := make([]dialPlan, 0)
	p.mu.RLock()
	connected := make(map[string]bool, len(p.links))
	for id, l := range p.links {
		connected[id] = l.ctx.Err() == nil
	}
	for _, seed := range p.config.Seeds {
		id := p.seedIDs[seed]
		if !connected[id] && !p.denied(id) {
			plans = append(plans, dialPlan{key: "seed:" + seed, seed: seed, id: id, endpoints: []string{seed}})
		}
	}
	p.mu.RUnlock()
	self := p.dir.Self().ID
	for _, n := range p.dir.Nodes() {
		if n.ID != self && !connected[n.ID] && !p.denied(n.ID) && len(n.Endpoints) > 0 {
			plans = append(plans, dialPlan{key: n.ID, id: n.ID, endpoints: n.Endpoints})
		}
	}
	now := time.Now()
	count := 0
	for _, plan := range plans {
		a := attempts[plan.key]
		if count == limit {
			break
		}
		if a.pending || now.Before(a.next) {
			continue
		}
		a.pending = true
		attempts[plan.key] = a
		count++
		p.wg.Go(func() { p.try(plan, results) })
	}
	return count
}

func (p *Peer) try(plan dialPlan, results chan<- dialResult) {
	sent := false
	report := func(ok bool) {
		if sent {
			return
		}
		sent = true
		select {
		case results <- dialResult{key: plan.key, ok: ok}:
		case <-p.ctx.Done():
		}
	}
	for _, endpoint := range plan.endpoints {
		p.connect(endpoint, plan.id, plan.seed, func() { report(true) })
		if sent {
			return
		}
		if p.ctx.Err() != nil {
			break
		}
	}
	report(false)
}

func (p *Peer) markDirty() {
	select {
	case p.dirty <- struct{}{}:
	default:
	}
}

func (p *Peer) importRecords(records []status.Node) error {
	records = slices.DeleteFunc(records, func(n status.Node) bool { return p.denied(n.ID) })
	changed, conflicts, err := p.dir.Merge(records)
	for _, id := range conflicts {
		p.log.Warn("conflicting node record", "node_id", id)
	}
	if changed {
		p.markDirty()
	}
	return err
}

func nodeHash(n status.Node) []byte {
	// The record has no maps, and addresses and endpoints are sorted on ingestion.
	b, _ := proto.Marshal(nodeToWire(n))
	h := sha256.Sum256(b)
	return h[:]
}

func (p *Peer) broadcastState() {
	p.broadcastReachability()
	inventory := &wire.Inventory{}
	for _, n := range p.dir.Nodes() {
		if !p.denied(n.ID) {
			inventory.Versions = append(inventory.Versions, &wire.RecordVersion{Id: n.ID, Revision: n.Revision, Hash: nodeHash(n)})
		}
	}
	p.mu.RLock()
	links := make([]*link, 0, len(p.links))
	for _, l := range p.links {
		links = append(links, l)
	}
	p.mu.RUnlock()
	for _, l := range links {
		for batch := range slices.Chunk(inventory.Versions, maxInventory) {
			l.send(&wire.Frame{Body: &wire.Frame_Inventory{Inventory: &wire.Inventory{Versions: batch}}})
		}
	}
}

func (l *link) receiveInventory(in *wire.Inventory) bool {
	if in == nil || len(in.Versions) > maxInventory {
		return false
	}
	p := l.owner
	nodes := make(map[string]status.Node)
	for _, n := range p.dir.Nodes() {
		nodes[n.ID] = n
	}
	self := p.dir.Self().ID
	var wanted []string
	for _, v := range in.Versions {
		if v == nil || !validID(v.Id) || v.Revision == 0 || len(v.Hash) != sha256.Size {
			return false
		}
		if p.denied(v.Id) {
			continue
		}
		old := nodes[v.Id]
		if old.Revision == v.Revision && !bytes.Equal(v.Hash, nodeHash(old)) {
			p.log.Warn("conflicting node version", "node_id", v.Id)
		}
		if v.Id != self && v.Revision > old.Revision {
			wanted = append(wanted, v.Id)
		}
	}
	for batch := range slices.Chunk(wanted, maxRecordBatch) {
		if !l.send(&wire.Frame{Body: &wire.Frame_RecordRequest{RecordRequest: &wire.RecordRequest{Ids: batch}}}) {
			return false
		}
	}
	return true
}

func (l *link) sendRecords(req *wire.RecordRequest) bool {
	if req == nil || len(req.Ids) > maxRecordBatch {
		return false
	}
	ids := make(map[string]bool, len(req.Ids))
	for _, id := range req.Ids {
		if !validID(id) {
			return false
		}
		ids[id] = true
	}
	out := &wire.Records{}
	frame := func() *wire.Frame { return &wire.Frame{Body: &wire.Frame_Records{Records: out}} }
	for _, n := range l.owner.dir.Nodes() {
		if !ids[n.ID] || l.owner.denied(n.ID) {
			continue
		}
		record := nodeToWire(n)
		out.Nodes = append(out.Nodes, record)
		if proto.Size(frame()) > maxFrame {
			out.Nodes = out.Nodes[:len(out.Nodes)-1]
			if len(out.Nodes) == 0 || !l.send(frame()) {
				return false
			}
			out = &wire.Records{Nodes: []*wire.NodeRecord{record}}
			if proto.Size(frame()) > maxFrame {
				return false
			}
		}
	}
	return len(out.Nodes) == 0 || l.send(frame())
}

func (l *link) receiveRecords(resp *wire.Records) bool {
	if resp == nil || len(resp.Nodes) > maxRecordBatch {
		return false
	}
	nodes := make([]status.Node, 0, len(resp.Nodes))
	for _, record := range resp.Nodes {
		n, err := nodeFromWire(record)
		if err != nil {
			return false
		}
		nodes = append(nodes, n)
	}
	if err := l.owner.importRecords(nodes); err != nil {
		l.owner.log.Error("save peer directory", "err", err)
		return false
	}
	return true
}
