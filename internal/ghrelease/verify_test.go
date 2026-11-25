package ghrelease

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// Public immutable release and real GitHub release signature; no signing key in tests.
var cliChecksum = identity{repo: "cli/cli", repoID: 212613049, tag: "v2.105.0", releaseID: 449359903, name: "gh_2.105.0_checksums.txt", digest: "4597138ea96e474c68001370c0a4955ff4834ba603c53900de6ebbef4cb0bed9"}

func fixture(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReleaseSignature(t *testing.T) {
	trust, err := root.NewTrustedRootFromJSON(fixture(t, "github-trusted-root.json"))
	if err != nil {
		t.Fatal(err)
	}
	data := fixture(t, "cli-release-bundle.json")
	if err := cliChecksum.verifyBundle(data, trust); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*identity){
		func(i *identity) { i.repo = "TakashiSasaki/hostmesh-mcp" }, func(i *identity) { i.repoID++ },
		func(i *identity) { i.tag = "v2.105.1" }, func(i *identity) { i.releaseID++ }, func(i *identity) { i.name = "peer" }, func(i *identity) { i.digest = "00" + i.digest[2:] },
	} {
		id := cliChecksum
		mutate(&id)
		if id.verifyBundle(data, trust) == nil {
			t.Fatal("accepted mismatched identity", id)
		}
	}
	var signed struct {
		DSSE struct{ Payload string } `json:"dsseEnvelope"`
	}
	if err := json.Unmarshal(data, &signed); err != nil {
		t.Fatal(err)
	}
	plain, err := base64.StdEncoding.DecodeString(signed.DSSE.Payload)
	if err != nil {
		t.Fatal(err)
	}
	altered := bytes.ReplaceAll(plain, []byte("cli/cli"), []byte("bad/bad"))
	bad := bytes.Replace(data, []byte(signed.DSSE.Payload), []byte(base64.StdEncoding.EncodeToString(altered)), 1)
	id := cliChecksum
	id.repo = "bad/bad"
	if id.verifyBundle(bad, trust) == nil {
		t.Fatal("accepted altered signed payload")
	}
}
