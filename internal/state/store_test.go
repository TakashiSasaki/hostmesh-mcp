package state

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"github.com/google/uuid"
)

func TestStorePersistsIdentityAndRejectsOlderRecord(t *testing.T) {
	d := t.TempDir()
	local := status.Node{Hostname: "local", OS: "linux"}
	s, err := Open(d, local)
	if err != nil {
		t.Fatal(err)
	}
	self := s.Self()
	r := status.Node{ID: uuid.NewString(), Hostname: "remote", OS: "linux", Revision: 3}
	if changed, _, err := s.Merge([]status.Node{r}); !changed || err != nil {
		t.Fatalf("merge = %v, %v", changed, err)
	}
	old := r
	old.Revision, old.Hostname = 2, "old"
	if changed, _, err := s.Merge([]status.Node{old}); changed || err != nil {
		t.Fatalf("stale merge = %v, %v", changed, err)
	}
	r.Hostname = "conflicting"
	if changed, conflicts, err := s.Merge([]status.Node{r}); changed || len(conflicts) != 1 || err != nil {
		t.Fatalf("conflicting merge = %v, %v, %v", changed, conflicts, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	local.Hostname = "renamed"
	s, err = Open(d, local)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := s.Self(); got.ID != self.ID || got.Revision != self.Revision+1 || got.Hostname != "renamed" {
		t.Fatalf("wrong reloaded self: %+v", got)
	}
	for _, n := range s.Nodes() {
		if n.ID == r.ID && n.Hostname == "remote" && n.Revision == 3 {
			return
		}
	}
	t.Fatal("remote record was not preserved")
}

func TestCorruptStateDoesNotCreateNewIdentity(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "state.json")
	if err := os.WriteFile(p, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(d, status.Node{Hostname: "local", OS: "linux"}); err == nil {
		s.Close()
		t.Fatal("corrupt state accepted")
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "{broken" {
		t.Fatalf("state overwritten: %q, %v", b, err)
	}
}
