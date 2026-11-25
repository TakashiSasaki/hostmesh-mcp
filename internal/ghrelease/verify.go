// Package ghrelease verifies GitHub's immutable release attestations.
package ghrelease

import (
	"context"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// GitHub's TUF bootstrap, from cli/cli v2.105.0,
// pkg/cmd/attestation/verification/embed/tuf-repo.github.com/root.json.
// TUF authenticates subsequent root rotations and current verification material.
//
//go:embed github-tuf-root.json
var githubRoot []byte

type identity struct {
	repo      string
	repoID    int64
	tag       string
	releaseID int64
	name      string
	digest    string
}

func githubTrust(ctx context.Context) (*root.TrustedRoot, error) {
	opts := tuf.DefaultOptions().WithContext(ctx).WithDisableLocalCache()
	opts.RepositoryBaseURL = "https://tuf-repo.github.com"
	opts.Root = githubRoot
	client, err := tuf.New(opts)
	if err != nil {
		return nil, err
	}
	return root.GetTrustedRoot(client)
}

func (id identity) verifyBundle(data []byte, trusted *root.TrustedRoot) error {
	var b bundle.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return err
	}
	verifier, err := verify.NewVerifier(trusted, verify.WithSignedTimestamps(1))
	if err != nil {
		return err
	}
	cert, err := verify.NewShortCertificateIdentity("", ".*", "https://dotcom.releases.github.com", "")
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(id.digest)
	if err != nil {
		return err
	}
	result, err := verifier.Verify(&b, verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(cert)))
	if err != nil {
		return err
	}
	s := result.Statement
	if s == nil || s.GetType() != "https://in-toto.io/Statement/v1" {
		return fmt.Errorf("expected GitHub release statement")
	}
	p := s.Predicate.GetFields()
	var attestedID string
	switch s.PredicateType {
	case "https://in-toto.io/attestation/release/v0.1":
		attestedID = p["releaseId"].GetStringValue()
	case "https://in-toto.io/attestation/release/v0.2":
		attestedID = p["databaseId"].GetStringValue()
	default:
		return fmt.Errorf("unsupported release predicate %q", s.PredicateType)
	}
	if p["repository"].GetStringValue() != id.repo || p["repositoryId"].GetStringValue() != strconv.FormatInt(id.repoID, 10) || attestedID != strconv.FormatInt(id.releaseID, 10) || p["tag"].GetStringValue() != id.tag {
		return fmt.Errorf("attested repository, release ID or tag mismatch")
	}
	for _, subject := range s.Subject {
		if subject.Name == id.name && subject.Digest["sha256"] == id.digest {
			return nil
		}
	}
	return fmt.Errorf("attestation does not bind %s to its SHA-256", id.name)
}
