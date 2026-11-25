package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type updateSettings struct {
	ManifestURL   string `json:"manifest_url"`
	PublicKeyFile string `json:"public_key_file"`
	Binary        string `json:"binary"`
	Unit          string `json:"systemd_unit"`
	UserService   bool   `json:"user_service"`
	Observer      string `json:"observer_mcp_url,omitempty"`
}

var unitName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*\.service$`)

func readUpdateSettings(path string) (updateSettings, error) {
	var c updateSettings
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := decodeUpdateJSON(b, &c); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.Binary) || !filepath.IsAbs(c.PublicKeyFile) || !unitName.MatchString(c.Unit) {
		return c, errors.New("set absolute binary and public_key_file paths and a systemd_unit ending in .service")
	}
	if _, err := updateURL(c.ManifestURL); err != nil {
		return c, err
	}
	if c.Observer != "" {
		if _, err := updateURL(c.Observer); err != nil {
			return c, fmt.Errorf("observer_mcp_url: %w", err)
		}
	}
	return c, nil
}

func decodeUpdateJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

type releaseArtifact struct {
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type releaseManifest struct {
	Product   string                     `json:"product"`
	Version   string                     `json:"version"`
	Artifacts map[string]releaseArtifact `json:"artifacts"`
}

func releaseVersion(v string) ([3]uint64, error) {
	var numbers [3]uint64
	parts := strings.Split(v, ".")
	if len(parts) == 3 {
		for i, p := range parts {
			n, err := strconv.ParseUint(p, 10, 64)
			if err != nil || strconv.FormatUint(n, 10) != p {
				return numbers, fmt.Errorf("invalid stable version %q", v)
			}
			numbers[i] = n
		}
		return numbers, nil
	}
	return numbers, fmt.Errorf("invalid stable version %q", v)
}

func updateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	ip, _ := netip.ParseAddr(u.Hostname())
	if u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && ip.IsLoopback())) {
		return nil, fmt.Errorf("expected HTTPS or a loopback HTTP URL: %q", raw)
	}
	return u, nil
}

var releaseClient = &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || (via[0].URL.Scheme == "https" && req.URL.Scheme != "https") {
		return errors.New("unsafe update redirect")
	}
	_, err := updateURL(req.URL.String())
	return err
}}

func fetchUpdate(ctx context.Context, raw string, dst io.Writer, limit int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, err
	}
	resp, err := releaseClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch %s: %s", req.URL.Redacted(), resp.Status)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, limit+1))
	if n > limit {
		return n, fmt.Errorf("update download exceeds %d bytes", limit)
	}
	return n, err
}

func loadRelease(ctx context.Context, c updateSettings) (string, releaseArtifact, error) {
	var a releaseArtifact
	pub, err := os.ReadFile(c.PublicKeyFile)
	if err != nil {
		return "", a, err
	}
	block, rest := pem.Decode(pub)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return "", a, errors.New("expected one Ed25519 PUBLIC KEY")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", a, err
	}
	ed, ok := key.(ed25519.PublicKey)
	if !ok {
		return "", a, errors.New("update signing key must be Ed25519")
	}
	u, err := updateURL(c.ManifestURL)
	if err != nil {
		return "", a, err
	}
	sigURL := *u
	sigURL.Path += ".sig"
	sigURL.RawPath = ""
	var manifest, sig bytes.Buffer
	if _, err := fetchUpdate(ctx, u.String(), &manifest, 1<<20); err != nil {
		return "", a, err
	}
	if _, err := fetchUpdate(ctx, sigURL.String(), &sig, ed25519.SignatureSize); err != nil {
		return "", a, err
	}
	if !ed25519.Verify(ed, manifest.Bytes(), sig.Bytes()) {
		return "", a, errors.New("invalid release manifest signature")
	}
	var m releaseManifest
	if err := decodeUpdateJSON(manifest.Bytes(), &m); err != nil {
		return "", a, err
	}
	if m.Product != "host-status-mcp" {
		return "", a, errors.New("unexpected release product")
	}
	if _, err := releaseVersion(m.Version); err != nil {
		return "", a, err
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	a, ok = m.Artifacts[platform]
	if !ok {
		return "", a, fmt.Errorf("no release artifact for %s", platform)
	}
	digest, err := hex.DecodeString(a.SHA256)
	if err != nil || len(digest) != sha256.Size || a.Size <= 0 || a.Size > 256<<20 || a.URL == "" {
		return "", a, errors.New("invalid artifact hash, size or URL")
	}
	resolved, err := u.Parse(a.URL)
	if err != nil {
		return "", a, err
	}
	if _, err := updateURL(resolved.String()); err != nil {
		return "", a, err
	}
	if u.Scheme == "https" && resolved.Scheme != "https" {
		return "", a, errors.New("HTTPS artifact required")
	}
	a.URL = resolved.String()
	return m.Version, a, nil
}

func stageRelease(ctx context.Context, path string, a releaseArtifact, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := fetchUpdate(ctx, a.URL, io.MultiWriter(f, h), a.Size)
	if err != nil {
		return err
	}
	if n != a.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), a.SHA256) {
		return errors.New("artifact size or SHA-256 mismatch")
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	return f.Sync()
}

func newerRelease(current, latest string) (bool, error) {
	old, err := releaseVersion(current)
	if err != nil {
		return false, err
	}
	next, err := releaseVersion(latest)
	return slices.Compare(next[:], old[:]) > 0, err
}
