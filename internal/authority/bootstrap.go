package authority

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Bootstrap creates a deployable online CA and a separate offline root in a
// new, private directory. Only the invocation that creates the directory owns it.
func Bootstrap(dir, serverName string) (err error) {
	now := time.Now().UTC()
	server := &x509.Certificate{
		Subject:   pkix.Name{CommonName: serverName},
		NotBefore: now.Add(-time.Minute), NotAfter: now.AddDate(0, 0, 180),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(serverName); ip != nil {
		server.IPAddresses = []net.IP{ip}
	} else if validDNSName(serverName) {
		server.DNSNames = []string{serverName}
	} else {
		return fmt.Errorf("invalid EST server DNS name or IP address %q", serverName)
	}

	if err := makePrivateDir(dir); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
				err = fmt.Errorf("%w (clean up incomplete CA directory: %v)", err, cleanupErr)
			}
		}
	}()
	offline, online := filepath.Join(dir, "offline"), filepath.Join(dir, "online")
	for _, sub := range []string{offline, online} {
		if err := os.Mkdir(sub, 0700); err != nil {
			return err
		}
	}
	caTemplate := func(name string, expires time.Time, depth int) *x509.Certificate {
		return &x509.Certificate{
			Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Minute), NotAfter: expires,
			IsCA: true, BasicConstraintsValid: true, MaxPathLen: depth, MaxPathLenZero: depth == 0,
			KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		}
	}
	cfg := ExampleConfig()
	root, err := generatePair(caTemplate("hostmesh-mcp Root CA", now.AddDate(10, 0, 0), 1), nil)
	if err != nil {
		return err
	}
	if err := root.write(offline, cfg.Roots, "root.key", nil); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(online, cfg.Roots), root.certPEM(), 0600); err != nil {
		return err
	}
	issuer, err := generatePair(caTemplate("hostmesh-mcp Issuing CA", now.AddDate(1, 0, 0), 0), root)
	if err != nil {
		return err
	}
	if err := issuer.write(online, cfg.IssuerCert, cfg.IssuerKey, nil); err != nil {
		return err
	}
	est, err := generatePair(server, issuer)
	if err != nil {
		return err
	}
	if err := est.write(online, cfg.ServerCert, cfg.ServerKey, issuer.certPEM()); err != nil {
		return err
	}
	if err := Init(filepath.Join(online, cfg.StateDir)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// Write the entry point last, after the key material and registry.
	return os.WriteFile(filepath.Join(online, "ca.json"), append(data, '\n'), 0600)
}

type generatedPair struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func generatePair(tpl *x509.Certificate, issuer *generatedPair) (*generatedPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	parent, signer := tpl, key
	if issuer != nil {
		parent, signer = issuer.cert, issuer.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, parent, &key.PublicKey, signer)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &generatedPair{cert: cert, key: key}, nil
}

func (p *generatedPair) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.cert.Raw})
}

func (p *generatedPair) write(dir, certFile, keyFile string, chain []byte) error {
	der, err := x509.MarshalPKCS8PrivateKey(p.key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, certFile), append(p.certPEM(), chain...), 0600)
}
