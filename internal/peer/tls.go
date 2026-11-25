package peer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/status"
	"google.golang.org/protobuf/proto"
)

type PeerTLS struct {
	config     *tls.Config
	id         string
	validUntil time.Time
}

func certificateID(c *x509.Certificate) (string, error) {
	const prefix = "urn:host-status-mcp:node:"
	var id string
	for _, uri := range c.URIs {
		value, ok := strings.CutPrefix(uri.String(), prefix)
		if !ok {
			continue
		}
		if id != "" || !validID(value) {
			return "", fmt.Errorf("invalid or ambiguous node URI SAN")
		}
		id = value
	}
	if id == "" || c.IsCA {
		return "", fmt.Errorf("expected a node leaf certificate with URI SAN %s<node_id>", prefix)
	}
	return id, nil
}

func chainExpiry(chain []*x509.Certificate) time.Time {
	until := chain[0].NotAfter
	for _, c := range chain[1:] {
		if c.NotAfter.Before(until) {
			until = c.NotAfter
		}
	}
	return until
}

func LoadPeerTLS(ca, cert, key string) (*PeerTLS, error) {
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no CA certificates in %s", ca)
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	id, err := certificateID(leaf)
	if err != nil {
		return nil, err
	}
	pair.Leaf = leaf
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, Certificates: []tls.Certificate{pair}}
	security := &PeerTLS{config: cfg, id: id}
	_, until, err := security.verifyChain(pair.Certificate, id)
	if err != nil {
		return nil, fmt.Errorf("verify local node certificate: %w", err)
	}
	security.validUntil = until
	return security, nil
}

func (s *PeerTLS) transport(server bool) *tls.Config {
	cfg := s.config.Clone()
	role := x509.ExtKeyUsageServerAuth
	if server {
		role = x509.ExtKeyUsageClientAuth
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 {
			return fmt.Errorf("missing verified node certificate")
		}
		leaf := cs.VerifiedChains[0][0]
		if !slices.Contains(leaf.ExtKeyUsage, role) {
			return fmt.Errorf("missing node certificate extended key usage")
		}
		_, err := certificateID(leaf)
		return err
	}
	return cfg
}

func (s *PeerTLS) NodeID() string { return s.id }

func (s *PeerTLS) verifyChain(raw [][]byte, id string) (*x509.Certificate, time.Time, error) {
	if len(raw) == 0 || len(raw) > 16 {
		return nil, time.Time{}, fmt.Errorf("invalid record certificate chain")
	}
	pool := x509.NewCertPool()
	var leaf *x509.Certificate
	for i, der := range raw {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, time.Time{}, err
		}
		if i == 0 {
			leaf = c
		} else {
			pool.AddCert(c)
		}
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		return nil, time.Time{}, fmt.Errorf("node certificate requires clientAuth or serverAuth")
	}
	certID, err := certificateID(leaf)
	if err != nil || certID != id {
		return nil, time.Time{}, fmt.Errorf("certificate node ID mismatch")
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: s.config.RootCAs, Intermediates: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}})
	if err != nil {
		return nil, time.Time{}, err
	}
	return leaf, chainExpiry(chains[0]), nil
}

// Sign a domain-separated, deterministic protobuf encoding of the record.
func recordBytes(n status.Node) []byte {
	n.Proof = nil
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(nodeToWire(n))
	if err != nil {
		return nil
	}
	return append([]byte("host-status-mcp:record:v1\x00"), payload...)
}

func (s *PeerTLS) Sign(n status.Node) (*status.RecordProof, error) {
	pair := s.config.Certificates[0]
	if n.ID != s.id || !time.Now().Before(s.validUntil) {
		return nil, fmt.Errorf("local certificate is expired or has a different node ID")
	}
	data := recordBytes(n)
	if data == nil {
		return nil, fmt.Errorf("invalid node record")
	}
	signer := pair.PrivateKey.(crypto.Signer)
	var opts crypto.SignerOpts = crypto.Hash(0)
	if _, ok := signer.Public().(ed25519.PublicKey); !ok {
		digest := sha256.Sum256(data)
		data = digest[:]
		opts = crypto.SHA256
	}
	sig, err := signer.Sign(rand.Reader, data, opts)
	if err != nil {
		return nil, err
	}
	return &status.RecordProof{Signature: sig, Chain: pair.Certificate, ValidUntil: s.validUntil}, nil
}

func (s *PeerTLS) Verify(n status.Node) (time.Time, error) {
	if n.Proof == nil {
		return time.Time{}, fmt.Errorf("unsigned node record")
	}
	leaf, until, err := s.verifyChain(n.Proof.Chain, n.ID)
	if err != nil {
		return time.Time{}, err
	}
	var alg x509.SignatureAlgorithm
	switch leaf.PublicKey.(type) {
	case ed25519.PublicKey:
		alg = x509.PureEd25519
	case *rsa.PublicKey:
		alg = x509.SHA256WithRSA
	case *ecdsa.PublicKey:
		alg = x509.ECDSAWithSHA256
	default:
		return time.Time{}, fmt.Errorf("unsupported signing key")
	}
	data := recordBytes(n)
	if data == nil || len(n.Proof.Signature) == 0 {
		return time.Time{}, fmt.Errorf("invalid signed record")
	}
	return until, leaf.CheckSignature(alg, data, n.Proof.Signature)
}
