//go:build linux

package status

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestNativePartialAndTotalFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	collect := Native([]string{missing})
	node := Node{OS: "linux"}

	got, err := collect(t.Context(), node, []string{"system", "filesystems"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sections.System == nil || len(got.Sections.Filesystems) != 0 || got.Errors["filesystems"] == "" ||
		got.Errors["filesystems:"+missing] == "" {
		t.Fatalf("unexpected partial result: %+v", got)
	}

	_, err = collect(t.Context(), node, []string{"filesystems"})
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != "COLLECTION_FAILED" {
		t.Fatalf("want COLLECTION_FAILED, got %v", err)
	}
}
