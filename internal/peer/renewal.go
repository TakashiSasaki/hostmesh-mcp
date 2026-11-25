package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	randv2 "math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/TakashiSasaki/host-status-mcp/internal/localfile"
	"github.com/smallstep/pkcs7"
)

// URL is the authenticated origin peer's EST simplereenroll endpoint.
// Gateway is a trusted operator-selected TCP destination at the exit peer.
type RenewalConfig struct {
	URL     string `json:"url,omitempty"`
	Relay   bool   `json:"relay,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

func (c RenewalConfig) offersRoute() bool { return c.Relay || c.Gateway != "" }

func (c RenewalConfig) Validate() error {
	if c.URL == "" && !c.offersRoute() {
		return errors.New("peer.renewal requires url, relay or gateway")
	}
	if c.Gateway != "" && !ValidEndpoint(c.Gateway) {
		return errors.New("invalid peer.renewal.gateway host:port")
	}
	if c.URL != "" {
		u, err := url.Parse(c.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			!strings.HasPrefix(u.Path, "/.well-known/est/") || !strings.HasSuffix(u.Path, "/simplereenroll") {
			return errors.New("peer.renewal.url must be an HTTPS EST simplereenroll URL")
		}
	}
	return nil
}

func (p *Peer) Renewed() <-chan struct{} { return p.renewed }

func (p *Peer) manageRenewal() {
	s := p.config.TLS
	// Start after two thirds of the effective lifetime, leaving outage margin.
	from := s.LocalCertificate().NotBefore
	delay := time.Until(s.validUntil.Add(-s.validUntil.Sub(from) / 3))
	var csr string
	backoff := time.Second
	for {
		timer := time.NewTimer(delay)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		ctx, cancel := context.WithTimeout(p.ctx, renewalTimeout)
		var retry time.Duration
		var err error
		// Repeat the same PKCS#10 request for EST pending approvals (HTTP 202).
		if csr == "" {
			csr, err = s.renewalCSR()
		}
		if err == nil {
			retry, err = p.renew(ctx, csr)
		}
		cancel()
		if err == nil {
			p.log.Info("renewed peer certificate", "node_id", s.id)
			close(p.renewed)
			return
		}
		if p.ctx.Err() != nil {
			return
		}
		delay = backoff + randv2.N(backoff/2)
		backoff = min(2*backoff, 5*time.Minute)
		if remaining := time.Until(s.validUntil); remaining > 0 {
			delay = min(delay, max(time.Second, remaining/5))
		}
		delay = max(delay, retry)
		p.log.Warn("peer certificate renewal failed", "err", err, "retry_in", delay)
	}
}

func (s *PeerTLS) renewalCSR() (string, error) {
	old := s.LocalCertificate()
	csr := &x509.CertificateRequest{RawSubject: old.RawSubject}
	for _, ext := range old.Extensions {
		if ext.Id.String() == "2.5.29.17" || ext.Id.Equal(seedExtensionOID) {
			csr.ExtraExtensions = append(csr.ExtraExtensions, ext)
		}
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, csr, s.config.Certificates[0].PrivateKey)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}

func (p *Peer) renew(ctx context.Context, csr string) (time.Duration, error) {
	s := p.config.TLS
	old := s.LocalCertificate()
	if !time.Now().Before(s.validUntil) {
		return 0, errors.New("expired certificate requires manual enrollment")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.config.Renewal.URL, strings.NewReader(csr))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/pkcs10")
	req.Header.Set("Accept", "application/pkcs7-mime")
	cfg := s.config.Clone()
	cfg.ServerName = req.URL.Hostname()
	// An authenticated mesh member must not impersonate the EST service, even
	// where multiple certificates carry the same host/IP SAN.
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		for _, uri := range cs.PeerCertificates[0].URIs {
			if strings.HasPrefix(uri.String(), "urn:host-status-mcp:node:") {
				return errors.New("EST server presented a peer identity")
			}
		}
		return nil
	}
	transport := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(ctx, renewalTimeout)
		defer cancel()
		p.mu.RLock()
		path := slices.Clone(p.renewalPathLocked())
		p.mu.RUnlock()
		return p.dialRenewal(ctx, path)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var wait time.Duration
		value := resp.Header.Get("Retry-After")
		if n, err := strconv.ParseUint(value, 10, 31); err == nil {
			wait = time.Duration(n) * time.Second
		} else if date, err := http.ParseTime(value); err == nil {
			wait = time.Until(date)
		}
		return wait, fmt.Errorf("EST reenrollment: HTTP %d", resp.StatusCode)
	}
	media, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "application/pkcs7-mime" || params["smime-type"] != "" && params["smime-type"] != "certs-only" {
		return 0, errors.New("expected EST certs-only response")
	}
	const limit = 192 << 10
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, err
	}
	if len(raw) > limit {
		return 0, errors.New("EST response too large")
	}
	encoded := strings.Join(strings.Fields(string(raw)), "")
	cms, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return 0, err
	}
	msg, err := pkcs7.Parse(cms)
	if err != nil {
		return 0, err
	}
	if len(msg.Signers) != 0 || len(msg.Content) != 0 {
		return 0, errors.New("expected a certs-only EST body")
	}
	chain, err := s.renewedChain(msg.Certificates)
	if err != nil {
		return 0, err
	}
	return 0, p.installRenewal(old, chain)
}

func (s *PeerTLS) renewedChain(certs []*x509.Certificate) ([]byte, error) {
	old := s.LocalCertificate()
	if len(certs) == 0 || len(certs) > 16 {
		return nil, errors.New("unexpected EST certificate count")
	}
	intermediates := x509.NewCertPool()
	var leaf *x509.Certificate
	for _, c := range certs {
		if c.IsCA {
			intermediates.AddCert(c)
			continue
		}
		if leaf != nil || !bytes.Equal(c.RawSubjectPublicKeyInfo, old.RawSubjectPublicKeyInfo) {
			return nil, errors.New("unexpected EST leaf or public key")
		}
		leaf = c
	}
	if leaf == nil {
		return nil, errors.New("missing EST leaf")
	}
	id, err := CertificateNodeID(leaf)
	if err != nil || id != s.id || !leaf.NotAfter.After(old.NotAfter) {
		return nil, errors.New("renewal must preserve node ID and extend certificate validity")
	}
	var roles []x509.ExtKeyUsage
	for _, role := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if slices.Contains(old.ExtKeyUsage, role) {
			if !slices.Contains(leaf.ExtKeyUsage, role) {
				return nil, errors.New("renewed certificate lost required peer usage")
			}
			roles = append(roles, role)
		}
	}
	for _, name := range old.DNSNames {
		if !slices.Contains(leaf.DNSNames, name) {
			return nil, errors.New("renewed certificate lost DNS SAN")
		}
	}
	for _, ip := range old.IPAddresses {
		if !slices.ContainsFunc(leaf.IPAddresses, ip.Equal) {
			return nil, errors.New("renewed certificate lost IP SAN")
		}
	}
	oldSeeds, err := certificateSeeds(old)
	if err != nil {
		return nil, err
	}
	newSeeds, err := certificateSeeds(leaf)
	if err != nil {
		return nil, err
	}
	if len(oldSeeds) > 0 && len(newSeeds) == 0 {
		return nil, errors.New("renewed certificate lost bootstrap seeds")
	}
	// Verify every required usage against the actual path we will write.
	// Different cross-signed intermediates can carry different restrictions.
	paths, err := leaf.Verify(x509.VerifyOptions{Roots: s.config.RootCAs, Intermediates: intermediates, KeyUsages: roles})
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		if !chainExpiry(path).After(s.validUntil) {
			continue
		}
		roots, issuers := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(path[len(path)-1])
		for i := 1; i < len(path)-1; i++ {
			issuers.AddCert(path[i])
		}
		allowed := true
		for _, role := range roles {
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: issuers, KeyUsages: []x509.ExtKeyUsage{role}}); err != nil {
				allowed = false
				break
			}
		}
		if allowed {
			var bundle bytes.Buffer
			for _, c := range path[:max(1, len(path)-1)] {
				pem.Encode(&bundle, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
			}
			return bundle.Bytes(), nil
		}
	}
	return nil, errors.New("no renewed chain extends usable lifetime with all required usages")
}

func (p *Peer) installRenewal(old *x509.Certificate, chain []byte) error {
	path, err := filepath.EvalSymlinks(p.config.CertFile)
	if err != nil {
		return err
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(current)
	if block == nil || !bytes.Equal(block.Bytes, old.Raw) {
		return errors.New("on-disk certificate changed during renewal")
	}
	for rest := current; len(bytes.TrimSpace(rest)) > 0; {
		var part *pem.Block
		part, rest = pem.Decode(rest)
		if part == nil || part.Type != "CERTIFICATE" {
			return errors.New("cert_file must contain only the public certificate chain for automatic renewal")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".renewal-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if _, err = f.Write(chain); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = localfile.Replace(f.Name(), path); err != nil {
		return err
	}
	if err = localfile.SyncDir(d); err != nil {
		// Rename has committed. Activate the on-disk identity even when the
		// filesystem cannot confirm durability across a later power failure.
		p.log.Warn("renewed certificate installed; directory sync failed", "err", err)
	}
	return nil
}
