package authority

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
)

func parseCSR(der []byte) (*x509.CertificateRequest, error) {
	if len(der) > 32<<10 {
		return nil, errors.New("CSR exceeds 32 KiB")
	}
	r, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	switch key := r.PublicKey.(type) {
	case ed25519.PublicKey, *ecdsa.PublicKey:
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 || key.N.BitLen() > 8192 {
			return nil, errors.New("unsupported RSA size")
		}
	default:
		return nil, errors.New("unsupported peer key algorithm")
	}
	if err = r.CheckSignature(); err != nil {
		return nil, err
	}
	return r, nil
}

func (a *Authority) issue(p Profile, public any) ([]*x509.Certificate, error) {
	id, err := keyID(public)
	if err != nil {
		return nil, err
	}
	if a.reserved[id] {
		return nil, errors.New("CA and EST service keys cannot be issued a peer certificate")
	}
	now := time.Now().UTC()
	notAfter := now.Add(a.lifetime)
	for _, c := range a.chain {
		if c.NotAfter.Before(notAfter) {
			notAfter = c.NotAfter
		}
	}
	if !notAfter.After(now) {
		return nil, errors.New("issuer chain has expired")
	}
	uri := &url.URL{Scheme: "urn", Opaque: "host-status-mcp:node:" + p.NodeID}
	c := &x509.Certificate{
		Subject:               pkix.Name{CommonName: p.NodeID},
		URIs:                  []*url.URL{uri},
		DNSNames:              p.DNS,
		NotBefore:             now.Add(-min(time.Minute, notAfter.Sub(now)/20)),
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if !p.ClientOnly {
		c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	for _, s := range p.IPs {
		c.IPAddresses = append(c.IPAddresses, net.ParseIP(s))
	}
	if len(p.Seeds) > 0 {
		ext, err := peer.BootstrapSeedExtension(p.Seeds)
		if err != nil {
			return nil, err
		}
		c.ExtraExtensions = []pkix.Extension{ext}
	}
	// x509 generates a fresh cryptographically random serial when SerialNumber is nil.
	der, err := x509.CreateCertificate(rand.Reader, c, a.chain[0], public, a.signer.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	issuers := a.chain[:len(a.chain)-1]
	for _, c := range issuers {
		pool.AddCert(c)
	}
	for _, usage := range leaf.ExtKeyUsage {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: a.roots, Intermediates: pool, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
			return nil, fmt.Errorf("issued certificate violates CA constraints: %w", err)
		}
	}
	return append([]*x509.Certificate{leaf}, issuers...), nil
}

// EnrollmentRequest represents a signature-verified request. Only its public key
// contributes to the certificate; identity and permissions come from the profile.
type EnrollmentRequest struct {
	request         *x509.CertificateRequest
	CSRSHA256       string `json:"csr_sha256"`
	PublicKeySHA256 string `json:"public_key_sha256"`
	Algorithm       string `json:"public_key_algorithm"`
}

func ParseEnrollmentRequest(data []byte) (*EnrollmentRequest, error) {
	data = bytes.TrimSpace(data)
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 ||
		!bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE REQUEST-----")) ||
		bytes.Count(data, []byte("-----BEGIN ")) != 1 {
		return nil, errors.New("expected a single PKCS#10 PEM CSR")
	}
	csr, err := parseCSR(block.Bytes)
	if err != nil {
		return nil, err
	}
	id, err := keyID(csr.PublicKey)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(csr.Raw)
	return &EnrollmentRequest{request: csr, CSRSHA256: hex.EncodeToString(digest[:]), PublicKeySHA256: id, Algorithm: csr.PublicKeyAlgorithm.String()}, nil
}

var (
	ErrKeyRevoked   = errors.New("key revoked")
	ErrNodeConflict = errors.New("conflicting node registration")
)

func (a *Authority) Enroll(p Profile, request *EnrollmentRequest) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	csr, id := request.request, request.PublicKeySHA256
	var out bytes.Buffer
	err := withRegistry(a.config.StateDir, true, func(path string) error {
		r, err := readRegistry(path)
		if err != nil {
			return err
		}
		if reg, ok := r.Keys[id]; ok {
			if reg.Revoked {
				return ErrKeyRevoked
			}
			if reg.Profile.NodeID != p.NodeID {
				return fmt.Errorf("%w: key bound to another node", ErrNodeConflict)
			}
		}
		for fingerprint, reg := range r.Keys {
			if !reg.Revoked && reg.Profile.NodeID == p.NodeID && fingerprint != id {
				return fmt.Errorf("%w: revoke the previous key before enrolling its replacement", ErrNodeConflict)
			}
		}
		chain, err := a.issue(p, csr.PublicKey)
		if err != nil {
			return err
		}
		for _, c := range chain {
			pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
		}
		r.Keys[id] = registration{Profile: p}
		return r.save(path)
	})
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
