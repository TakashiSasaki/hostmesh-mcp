package authority

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/peer"
	"github.com/smallstep/pkcs7"
)

const endpoint = "/.well-known/est/simplereenroll"

var errDenied = errors.New("reenrollment not authorized")
var errLifetime = errors.New("issuer cannot extend validity")

func (a *Authority) reenroll(old *x509.Certificate, csr *x509.CertificateRequest) ([]byte, error) {
	node, err := peer.CertificateNodeID(old)
	if err != nil || !slices.Contains(old.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return nil, errDenied
	}
	if len(csr.URIs) > 0 {
		requested, err := peer.CertificateNodeID(&x509.Certificate{URIs: csr.URIs})
		if err != nil || node != requested {
			return nil, errDenied
		}
	}
	id, err := keyID(old.PublicKey)
	if err != nil {
		return nil, err
	}
	requestedKey, err := keyID(csr.PublicKey)
	if err != nil || requestedKey != id {
		return nil, errDenied
	}
	var reply []byte
	err = withRegistry(a.config.StateDir, false, func(path string) error {
		r, err := readRegistry(path)
		if err != nil {
			return err
		}
		reg, ok := r.Keys[id]
		if !ok || reg.Revoked || reg.Profile.NodeID != node {
			return errDenied
		}
		chain, err := a.issue(reg.Profile, csr.PublicKey)
		if err != nil {
			return err
		}
		if !chain[0].NotAfter.After(old.NotAfter) {
			return errLifetime
		}
		var der []byte
		for _, c := range chain {
			der = append(der, c.Raw...)
		}
		reply, err = pkcs7.DegenerateCertificate(der)
		return err
	})
	return reply, err
}

func (a *Authority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != endpoint {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "POST required", 405)
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		http.Error(w, "client certificate required", 403)
		return
	}
	chain := r.TLS.VerifiedChains[0]
	now := time.Now()
	for _, c := range chain {
		if now.Before(c.NotBefore) || !now.Before(c.NotAfter) {
			http.Error(w, "expired or invalid certificate", 403)
			return
		}
	}
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/pkcs10" {
		http.Error(w, "expected application/pkcs10", 415)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 48<<10))
	if err != nil {
		http.Error(w, "invalid or oversized request", 413)
		return
	}
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		http.Error(w, "invalid base64", 400)
		return
	}
	csr, err := parseCSR(der)
	if err != nil {
		http.Error(w, "invalid signed CSR", 400)
		return
	}
	cms, err := a.reenroll(chain[0], csr)
	switch {
	case errors.Is(err, errDenied):
		http.Error(w, "renewal denied", 403)
	case errors.Is(err, errLifetime):
		http.Error(w, "cannot extend certificate lifetime", 409)
	case err != nil:
		slog.Error("CA renewal failed", "err", err)
		http.Error(w, "issuance unavailable", 503)
	default:
		w.Header().Set("Content-Type", "application/pkcs7-mime; smime-type=certs-only")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, base64.StdEncoding.EncodeToString(cms))
	}
}

func (a *Authority) checkRegistry(path string) error {
	r, err := readRegistry(path)
	if err != nil {
		return err
	}
	for key := range a.reserved {
		if _, registered := r.Keys[key]; registered {
			return errors.New("CA and EST service keys must be separate from registered peer keys")
		}
	}
	return nil
}

// Check validates the service identities and any existing registry without binding
// a socket or creating state. A new installation can run it before ca init.
func (a *Authority) Check() error {
	if _, err := a.serverTLS(); err != nil {
		return err
	}
	err := a.checkRegistry(filepath.Join(a.config.StateDir, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (a *Authority) Serve(ctx context.Context, log io.Writer) error {
	return a.ServeReady(ctx, log, func(string) {})
}

// ServeReady calls ready after the EST listener is bound.
func (a *Authority) ServeReady(ctx context.Context, log io.Writer, ready func(string)) error {
	if err := withRegistry(a.config.StateDir, false, a.checkRegistry); err != nil {
		return err
	}
	cfg, err := a.serverTLS()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", a.config.Listen)
	if err != nil {
		return err
	}
	defer l.Close()
	srv := &http.Server{
		Handler:           a,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	fmt.Fprintln(log, "CA listening on", l.Addr())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(tls.NewListener(l, cfg)) }()
	ready(l.Addr().String())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			srv.Close()
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
