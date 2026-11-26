package authority

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/localfile"
	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
	"github.com/google/uuid"
)

// Profile is the local administrator's authorization, independent of CSR claims.
type Profile struct {
	NodeID     string   `json:"node_id"`
	DNS        []string `json:"dns_names,omitempty"`
	IPs        []string `json:"ip_addresses,omitempty"`
	Seeds      []string `json:"seeds,omitempty"`
	ClientOnly bool     `json:"client_only,omitempty"`
}

func ReadProfile(path string) (Profile, error) {
	var p Profile
	if err := readJSON(path, &p); err != nil {
		return p, err
	}
	return p, p.validate()
}

func (p Profile) validate() error {
	u, err := uuid.Parse(p.NodeID)
	if err != nil || u.Version() != 4 || u.String() != p.NodeID {
		return errors.New("expected canonical version-4 node_id")
	}
	if len(p.DNS)+len(p.IPs) > 128 {
		return errors.New("too many names")
	}
	for _, name := range p.DNS {
		if !validDNSName(name) {
			return fmt.Errorf("invalid DNS name %q", name)
		}
	}
	for _, s := range p.IPs {
		ip, err := netip.ParseAddr(s)
		if err != nil || ip.Zone() != "" {
			return fmt.Errorf("invalid IP address %q", s)
		}
	}
	if len(p.Seeds) > 0 {
		if _, err := peer.BootstrapSeedExtension(p.Seeds); err != nil {
			return err
		}
	}
	return nil
}

func validDNSName(name string) bool {
	if !peer.ValidEndpoint(net.JoinHostPort(name, "443")) || strings.Contains(name, ":") || net.ParseIP(name) != nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

type registration struct {
	Profile Profile `json:"profile"`
	Revoked bool    `json:"revoked,omitempty"`
}
type registry struct {
	Keys map[string]registration `json:"keys"`
}

func keyID(public any) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

// Use a stable sibling lock file. The registry inode changes on atomic rename.
func withRegistry(dir string, exclusive bool, f func(string) error) error {
	lock, err := os.OpenFile(filepath.Join(dir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := localfile.Lock(lock, exclusive, true); err != nil {
		return err
	}
	return f(filepath.Join(dir, "registry.json"))
}

func readRegistry(path string) (registry, error) {
	var r registry
	if err := readJSON(path, &r); err != nil {
		return r, err
	}
	if r.Keys == nil {
		return r, errors.New("invalid authority registry")
	}
	active := map[string]bool{}
	for key, reg := range r.Keys {
		digest, err := hex.DecodeString(key)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != key {
			return r, errors.New("invalid registry fingerprint")
		}
		if err := reg.Profile.validate(); err != nil {
			return r, err
		}
		if !reg.Revoked {
			if active[reg.Profile.NodeID] {
				return r, errors.New("ambiguous active node registration")
			}
			active[reg.Profile.NodeID] = true
		}
	}
	return r, nil
}

func (r registry) save(path string) error {
	dir := filepath.Dir(path)
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := os.CreateTemp(dir, ".registry-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(r); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = localfile.Replace(f.Name(), path); err != nil {
		return err
	}
	return localfile.SyncDir(d)
}

func Init(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return withRegistry(dir, true, func(path string) error {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("registry already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return (registry{Keys: map[string]registration{}}).save(path)
	})
}

func Revoke(dir, id string, certPEM []byte) error {
	var fingerprint string
	if certPEM != nil {
		cert, err := firstCertificate(certPEM)
		if err != nil {
			return err
		}
		id, err = peer.CertificateNodeID(cert)
		if err != nil {
			return err
		}
		fingerprint, err = keyID(cert.PublicKey)
		if err != nil {
			return err
		}
	}
	return withRegistry(dir, true, func(path string) error {
		r, err := readRegistry(path)
		if err != nil {
			return err
		}
		found := false
		for key, reg := range r.Keys {
			if reg.Profile.NodeID == id && (fingerprint == "" || fingerprint == key) {
				found = true
				if !reg.Revoked {
					reg.Revoked = true
					r.Keys[key] = reg
					return r.save(path)
				}
			}
		}
		if !found {
			return errors.New("unknown node or key")
		}
		return nil
	})
}
