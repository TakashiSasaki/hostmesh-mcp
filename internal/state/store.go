package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

type RecordTrust interface {
	NodeID() string
	Sign(status.Node) (*status.RecordProof, error)
	Verify(status.Node) (time.Time, error)
}
type snapshot struct {
	Authenticated bool                           `json:"authenticated,omitempty"`
	Proofs        map[string]*status.RecordProof `json:"proofs,omitempty"`
	SelfID        string                         `json:"self_id"`
	Nodes         map[string]status.Node         `json:"nodes"`
}

type Store struct {
	trust RecordTrust
	mu    sync.RWMutex
	dir   string
	lock  *os.File
	data  snapshot
}

func LocalNode(endpoints []string, relay bool) (status.Node, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return status.Node{}, err
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return status.Node{}, err
	}
	ips := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		p, err := netip.ParsePrefix(addr.String())
		if err == nil && p.Addr().IsGlobalUnicast() {
			ips = append(ips, p.Addr().Unmap().String())
		}
	}
	sort.Strings(ips)
	endpoints = slices.Clone(endpoints)
	sort.Strings(endpoints)
	return status.Node{Hostname: hostname, OS: runtime.GOOS, Addresses: slices.Compact(ips), Endpoints: slices.Compact(endpoints), Relay: relay}, nil
}

func Open(dir string, local status.Node) (*Store, error) {
	return OpenWithTrust(dir, local, nil)
}

// OpenLocal preserves a peering ledger when the identity is used over stdio.
func OpenLocal(dir string, local status.Node) (*Store, error) {
	return open(dir, local, nil, true)
}

func OpenWithTrust(dir string, local status.Node, trust RecordTrust) (*Store, error) {
	return open(dir, local, trust, false)
}
func open(dir string, local status.Node, trust RecordTrust, localOnly bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "state.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	s := &Store{dir: dir, lock: lock, trust: trust}
	if err := s.load(local, localOnly); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.lock.Close() }

func (s *Store) load(local status.Node, localOnly bool) error {
	b, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		local.ID, local.Revision = id.String(), 1
		if s.trust != nil {
			local.ID = s.trust.NodeID()
			local.Proof, err = s.trust.Sign(local)
			if err != nil {
				return err
			}
		}
		return s.commit(snapshot{SelfID: local.ID, Nodes: map[string]status.Node{local.ID: local}})
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	self := s.data.Nodes[s.data.SelfID]
	if _, err := uuid.Parse(s.data.SelfID); err != nil || self.ID != s.data.SelfID || self.Revision == 0 {
		return fmt.Errorf("invalid persistent node identity")
	}
	if localOnly && s.data.Authenticated {
		local.ID, local.Revision = self.ID, self.Revision
		s.data.Nodes = map[string]status.Node{self.ID: local}
		return nil
	}
	if s.trust != nil && s.trust.NodeID() != self.ID {
		return fmt.Errorf("local certificate node ID does not match persisted identity %s", self.ID)
	}
	modeChanged := s.data.Authenticated != (s.trust != nil)
	if modeChanged {
		s.data.Nodes = map[string]status.Node{self.ID: self}
	}
	if s.trust != nil {
		for id, n := range s.data.Nodes {
			if id == self.ID {
				continue
			}
			n.Proof = s.data.Proofs[id]
			until, verifyErr := s.trust.Verify(n)
			if verifyErr != nil || id != n.ID {
				delete(s.data.Nodes, id)
				continue
			}
			n.Proof.ValidUntil = until
			s.data.Nodes[id] = n
		}
	}
	_, err = s.updateSelf(local, modeChanged || s.trust != nil)
	return err
}

func (s *Store) commit(next snapshot) error {
	next.Authenticated = s.trust != nil
	if next.Authenticated {
		next.Proofs = make(map[string]*status.RecordProof, len(next.Nodes))
		for id, n := range next.Nodes {
			next.Proofs[id] = n.Proof
		}
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return err
	}
	s.data = next
	return nil
}

func (s *Store) Self() status.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Nodes[s.data.SelfID]
}

func (s *Store) Nodes() []status.Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	nodes := slices.Collect(maps.Values(s.data.Nodes))
	if s.trust != nil {
		now := time.Now()
		nodes = slices.DeleteFunc(nodes, func(n status.Node) bool {
			return n.ID != s.data.SelfID && (n.Proof == nil || !now.Before(n.Proof.ValidUntil))
		})
	}
	slices.SortFunc(nodes, func(a, b status.Node) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return nodes
}

func (s *Store) UpdateSelf(node status.Node) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateSelf(node, false)
}

func (s *Store) updateSelf(node status.Node, force bool) (bool, error) {
	old := s.data.Nodes[s.data.SelfID]
	old.Proof = nil
	node.ID, node.Revision = old.ID, old.Revision
	if !force && reflect.DeepEqual(node, old) {
		return false, nil
	}
	node.Revision++
	if s.trust != nil {
		proof, err := s.trust.Sign(node)
		if err != nil {
			return false, err
		}
		node.Proof = proof
	}
	nodes := maps.Clone(s.data.Nodes)
	nodes[node.ID] = node
	err := s.commit(snapshot{SelfID: node.ID, Nodes: nodes})
	return err == nil, err
}

func (s *Store) Merge(in []status.Node) (bool, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := maps.Clone(s.data.Nodes)
	changed := false
	var conflicts []string
	for _, node := range in {
		if s.trust != nil {
			until, err := s.trust.Verify(node)
			if err != nil {
				return false, nil, err
			}
			proof := *node.Proof
			proof.ValidUntil = until
			node.Proof = &proof
		} else if node.Proof != nil {
			return false, nil, fmt.Errorf("plaintext ledger rejects a record with a certificate or signature")
		}
		old := nodes[node.ID]
		if old.Revision == node.Revision && old.Revision != 0 && !reflect.DeepEqual(old, node) {
			conflicts = append(conflicts, node.ID)
		}
		if node.ID != s.data.SelfID && node.Revision > old.Revision {
			nodes[node.ID] = node
			changed = true
		}
	}
	if !changed {
		return false, conflicts, nil
	}
	err := s.commit(snapshot{SelfID: s.data.SelfID, Nodes: nodes})
	return err == nil, conflicts, err
}
