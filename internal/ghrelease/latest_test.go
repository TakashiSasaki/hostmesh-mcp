package ghrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/sigstore/sigstore-go/pkg/root"
)

func TestImmutableStableAsset(t *testing.T) {
	const name = "hostmesh-mcp-0.0.30-linux-arm64"
	a := Asset{Name: name, Size: 100, Digest: "sha256:" + strings.Repeat("a", 64), URL: "https://github.com/" + repository + "/releases/download/v0.0.30/" + name}
	r := release{ID: 42, Immutable: true, Tag: "v0.0.30", Assets: []Asset{a}}
	got, id, err := r.linuxAsset("arm64")
	if err != nil || got != a || id.releaseID != 42 || id.repoID != repositoryID {
		t.Fatal(got, id, err)
	}
	for _, mutate := range []func(*release){
		func(r *release) { r.Immutable = false }, func(r *release) { r.Prerelease = true }, func(r *release) { r.Draft = true },
		func(r *release) { r.Tag = "v00.0.30" }, func(r *release) { r.Tag = "v0.0.30-beta" }, func(r *release) { r.Assets[0].URL = "https://other.example/binary" },
		func(r *release) { r.Assets[0].Name = "wrong" }, func(r *release) { r.Assets[0].Size = 0 }, func(r *release) { r.Assets[0].Digest = "md5:1234" },
	} {
		bad := r
		bad.Assets = []Asset{a}
		mutate(&bad)
		if _, _, err := bad.linuxAsset("arm64"); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestAttestationAPI(t *testing.T) {
	const blob = "https://storage.example/bundle.sn"
	id := cliChecksum
	wantAPI := "https://api.github.com/repos/cli/cli/attestations/sha256:" + id.digest + "?predicate_type=release&per_page=100"
	trust := func(context.Context) (*root.TrustedRoot, error) {
		return root.NewTrustedRootFromJSON(fixture(t, "github-trusted-root.json"))
	}
	fetch := func(_ context.Context, url string, limit int64) ([]byte, error) {
		if limit != maxMetadata {
			t.Fatal(limit)
		}
		switch url {
		case wantAPI:
			return json.Marshal(map[string]any{"attestations": []attestation{{Initiator: "github", BundleURL: blob}}})
		case blob:
			return snappy.Encode(nil, fixture(t, "cli-release-bundle.json")), nil
		default:
			return nil, fmt.Errorf("unexpected URL %q", url)
		}
	}
	if err := id.checkAttestations(t.Context(), fetch, trust); err != nil {
		t.Fatal(err)
	}
}
