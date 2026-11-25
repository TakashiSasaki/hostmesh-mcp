package ghrelease

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Exercise the public API, blob transport, live TUF root rotation, timestamp,
// signature and actual downloaded bytes without publishing a hostmesh release.
func TestPublicImmutableRelease(t *testing.T) {
	if os.Getenv("HOSTMESH_LIVE_RELEASE_TEST") != "1" {
		t.Skip("opt-in live GitHub release verification")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	fetch := func(ctx context.Context, url string, limit int64) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: status %d", req.URL.Host, res.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
		if int64(len(data)) > limit {
			return nil, fmt.Errorf("oversize response")
		}
		return data, err
	}
	id := cliChecksum
	if err := id.checkAttestations(ctx, fetch, githubTrust); err != nil {
		t.Fatal(err)
	}
	data, err := fetch(ctx, "https://github.com/"+id.repo+"/releases/download/"+id.tag+"/"+id.name, maxMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != id.digest {
		t.Fatal("downloaded digest mismatch")
	}
}
