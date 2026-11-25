package ghrelease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/root"
)

const repository = "TakashiSasaki/hostmesh-mcp"
const repositoryID = 1471516634
const maxMetadata = 2 << 20

var stableTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

type release struct {
	ID         int64   `json:"id"`
	Tag        string  `json:"tag_name"`
	Immutable  bool    `json:"immutable"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

type fetchFunc func(context.Context, string, int64) ([]byte, error)

// Latest returns only an asset bound to this repository and release by GitHub's signature.
func Latest(ctx context.Context, arch string, fetch fetchFunc) (string, Asset, error) {
	var r release
	raw, err := fetch(ctx, "https://api.github.com/repos/"+repository+"/releases/latest", maxMetadata)
	if err != nil {
		return "", Asset{}, fmt.Errorf("GitHub latest release: %w", err)
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return "", Asset{}, err
	}
	asset, id, err := r.linuxAsset(arch)
	if err != nil {
		return "", Asset{}, err
	}
	if err := id.checkAttestations(ctx, fetch, githubTrust); err != nil {
		return "", Asset{}, err
	}
	return strings.TrimPrefix(r.Tag, "v"), asset, nil
}

func (r release) linuxAsset(arch string) (Asset, identity, error) {
	if !r.Immutable || r.Draft || r.Prerelease || r.ID <= 0 || !stableTag.MatchString(r.Tag) {
		return Asset{}, identity{}, fmt.Errorf("expected published immutable stable release, got %q", r.Tag)
	}
	if arch != "amd64" && arch != "arm64" {
		return Asset{}, identity{}, fmt.Errorf("unsupported Linux architecture %q", arch)
	}
	name := "hostmesh-mcp-" + r.Tag[1:] + "-linux-" + arch
	wantURL := "https://github.com/" + repository + "/releases/download/" + r.Tag + "/" + name
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		hash, ok := strings.CutPrefix(a.Digest, "sha256:")
		b, err := hex.DecodeString(hash)
		if !ok || err != nil || len(b) != sha256.Size || a.URL != wantURL || a.Size <= 0 || a.Size > 256<<20 {
			return Asset{}, identity{}, fmt.Errorf("invalid GitHub asset metadata for %s", name)
		}
		return a, identity{repo: repository, repoID: repositoryID, tag: r.Tag, releaseID: r.ID, name: name, digest: hash}, nil
	}
	return Asset{}, identity{}, fmt.Errorf("missing release asset %s", name)
}

type attestation struct {
	Initiator string          `json:"initiator"`
	Bundle    json.RawMessage `json:"bundle"`
	BundleURL string          `json:"bundle_url"`
}

func (a attestation) bytes(ctx context.Context, fetch fetchFunc) ([]byte, error) {
	if a.BundleURL == "" {
		return a.Bundle, nil
	}
	u, err := url.Parse(a.BundleURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("invalid GitHub bundle URL")
	}
	data, err := fetch(ctx, a.BundleURL, maxMetadata)
	if err != nil {
		return nil, err
	}
	// GitHub's bundle_url serves Snappy block format. Bound allocation before decoding.
	size, err := snappy.DecodedLen(data)
	if err != nil || size > maxMetadata {
		return nil, errors.New("invalid compressed attestation")
	}
	return snappy.Decode(nil, data)
}

func (id identity) checkAttestations(ctx context.Context, fetch fetchFunc, trust func(context.Context) (*root.TrustedRoot, error)) error {
	endpoint := "https://api.github.com/repos/" + id.repo + "/attestations/sha256:" + id.digest + "?predicate_type=release&per_page=100"
	raw, err := fetch(ctx, endpoint, maxMetadata)
	if err != nil {
		return err
	}
	var list struct {
		Attestations []attestation `json:"attestations"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	lastErr := errors.New("no matching GitHub release attestation")
	var trusted *root.TrustedRoot
	for _, a := range list.Attestations {
		if a.Initiator != "github" {
			continue
		}
		if trusted == nil {
			trusted, err = trust(ctx)
			if err != nil {
				return fmt.Errorf("load GitHub trust root: %w", err)
			}
		}
		data, err := a.bytes(ctx, fetch)
		if err == nil {
			err = id.verifyBundle(data, trusted)
		}
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return fmt.Errorf("cannot verify %s at %s: %w", id.name, id.tag, lastErr)
}
