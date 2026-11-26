// Package authority provides the local, operator-managed EST issuing authority.
package authority

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
)

type Config struct {
	Listen     string `json:"listen"`
	Roots      string `json:"ca_file"`
	IssuerCert string `json:"issuer_cert_file"`
	IssuerKey  string `json:"issuer_key_file"`
	ServerCert string `json:"server_cert_file"`
	ServerKey  string `json:"server_key_file"`
	StateDir   string `json:"state_dir"`
	Lifetime   string `json:"lifetime"`
}

func ExampleConfig() Config {
	return Config{
		Listen: "127.0.0.1:9443", Roots: "root.pem",
		IssuerCert: "issuer.pem", IssuerKey: "issuer.key",
		ServerCert: "est-chain.pem", ServerKey: "est.key",
		StateDir: "state", Lifetime: "720h",
	}
}

func readJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func ReadConfig(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:9443", Lifetime: "720h"}
	if err := readJSON(path, &c); err != nil {
		return c, err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	ip, ipErr := netip.ParseAddr(host)
	_, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || ipErr != nil || !ip.IsLoopback() || portErr != nil {
		return c, errors.New("CA listen must use a loopback IP and numeric port")
	}
	duration, err := time.ParseDuration(c.Lifetime)
	if err != nil || duration < time.Second {
		return c, errors.New("invalid positive certificate lifetime (minimum 1s)")
	}
	for _, v := range []*string{&c.Roots, &c.IssuerCert, &c.IssuerKey, &c.ServerCert, &c.ServerKey, &c.StateDir} {
		if *v == "" {
			return c, errors.New("CA configuration requires ca_file, issuer_cert_file, issuer_key_file, server_cert_file, server_key_file and state_dir")
		}
		if !filepath.IsAbs(*v) {
			*v = filepath.Join(filepath.Dir(path), *v)
		}
	}
	return c, nil
}

type Authority struct {
	rootsPEM string
	config   Config
	roots    *x509.CertPool
	signer   tls.Certificate
	chain    []*x509.Certificate // intermediate through trust anchor
	lifetime time.Duration
	reserved map[string]bool
	serverID string
}

// PublicRoots returns only the public certificates verified when the CA was loaded.
func (a *Authority) PublicRoots() string { return a.rootsPEM }

func loadPair(cert, key string) (tls.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err == nil && pair.Leaf == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	}
	return pair, err
}

func firstCertificate(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("expected a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func Load(c Config) (*Authority, error) {
	b, err := os.ReadFile(c.Roots)
	if err != nil {
		return nil, err
	}
	var rootsPEM bytes.Buffer
	roots := x509.NewCertPool()
	pair, err := loadPair(c.IssuerCert, c.IssuerKey)
	if err != nil {
		return nil, err
	}
	reserved := make(map[string]bool)
	signerID, err := keyID(pair.Leaf.PublicKey)
	if err != nil {
		return nil, err
	}
	for len(bytes.TrimSpace(b)) != 0 {
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid CA trust bundle")
		}
		root, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		rootID, err := keyID(root.PublicKey)
		if err != nil {
			return nil, err
		}
		reserved[rootID] = true
		if !root.IsCA || rootID == signerID {
			return nil, errors.New("use an intermediate signer distinct from the trusted root keys")
		}
		roots.AddCert(root)
		if err := pem.Encode(&rootsPEM, &pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}); err != nil {
			return nil, err
		}
		b = rest
	}
	if !pair.Leaf.IsCA || pair.Leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("issuer must be a certificate-signing intermediate CA")
	}
	chains, err := verifyPair(pair, roots, x509.ExtKeyUsageAny)
	if err != nil {
		return nil, fmt.Errorf("issuer chain: %w", err)
	}
	lifetime, err := time.ParseDuration(c.Lifetime)
	if err != nil {
		return nil, err
	}
	for _, cert := range chains[0] {
		key, err := keyID(cert.PublicKey)
		if err != nil {
			return nil, err
		}
		reserved[key] = true
	}
	data, err := os.ReadFile(c.ServerCert)
	if err != nil {
		return nil, err
	}
	server, err := firstCertificate(data)
	if err != nil {
		return nil, err
	}
	serverID, err := keyID(server.PublicKey)
	if err != nil {
		return nil, err
	}
	if reserved[serverID] {
		return nil, errors.New("EST server must use a dedicated key")
	}
	reserved[serverID] = true
	return &Authority{config: c, rootsPEM: rootsPEM.String(), roots: roots, signer: pair, chain: chains[0], lifetime: lifetime, reserved: reserved, serverID: serverID}, nil
}

func verifyPair(pair tls.Certificate, roots *x509.CertPool, usage x509.ExtKeyUsage) ([][]*x509.Certificate, error) {
	pool := x509.NewCertPool()
	for _, raw := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, err
		}
		pool.AddCert(c)
	}
	return pair.Leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: pool, KeyUsages: []x509.ExtKeyUsage{usage}})
}

func (a *Authority) serverTLS() (*tls.Config, error) {
	p, err := loadPair(a.config.ServerCert, a.config.ServerKey)
	if err != nil {
		return nil, err
	}
	id, err := keyID(p.Leaf.PublicKey)
	if err != nil || id != a.serverID {
		return nil, errors.New("EST server identity changed while loading configuration")
	}
	if p.Leaf.IsCA || len(p.Leaf.DNSNames)+len(p.Leaf.IPAddresses) == 0 {
		return nil, errors.New("EST requires a dedicated server leaf with DNS or IP SAN")
	}
	if peer.HasNodeURI(p.Leaf) {
		return nil, errors.New("EST server must have a separate identity from a mesh peer")
	}
	_, err = verifyPair(p, a.roots, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("EST server chain: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: a.roots, Certificates: []tls.Certificate{p}}, nil
}
