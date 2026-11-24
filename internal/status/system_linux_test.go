//go:build linux

package status

import (
	"testing"
	"testing/fstest"
)

func TestReadOSRelease(t *testing.T) {
	t.Run("prefer etc and decode quotes", func(t *testing.T) {
		root := fstest.MapFS{
			"etc/os-release": {Data: []byte(`# Test distribution
ID=lab-linux
NAME="Lab \"Linux\""
VERSION_ID='2.4'
VERSION="old"
VERSION="2.4 (Test) \\\\ \$lab"
`)},
			"usr/lib/os-release": {Data: []byte("VERSION_ID=9\n")},
		}
		var got System
		if err := readOSRelease(root, &got); err != nil {
			t.Fatal(err)
		}
		if got.OSID != "lab-linux" || got.OSName != `Lab "Linux"` ||
			got.OSVersionID != "2.4" || got.OSVersion != `2.4 (Test) \\ $lab` {
			t.Fatalf("unexpected distribution: %+v", got)
		}
	})
	t.Run("fall back only if missing", func(t *testing.T) {
		root := fstest.MapFS{
			"usr/lib/os-release": {Data: []byte("PRETTY_NAME='Lab Linux'\nVERSION_ID=2.4\n")},
		}
		var got System
		if err := readOSRelease(root, &got); err != nil {
			t.Fatal(err)
		}
		if got.OSName != "Lab Linux" || got.OSVersion != "2.4" || got.OSVersionID != "2.4" {
			t.Fatalf("unexpected fallback distribution: %+v", got)
		}
		root["etc/os-release"] = &fstest.MapFile{Data: []byte("ID=rolling\n")}
		got = System{}
		if err := readOSRelease(root, &got); err != nil {
			t.Fatal(err)
		}
		if got.OSID != "rolling" || got.OSName != "" || got.OSVersion != "" || got.OSVersionID != "" {
			t.Fatalf("unexpected rolling distribution: %+v", got)
		}
	})
	t.Run("without metadata", func(t *testing.T) {
		got := System{KernelRelease: "6.17.0"}
		if err := readOSRelease(fstest.MapFS{}, &got); err != nil {
			t.Fatal(err)
		}
		if got != (System{KernelRelease: "6.17.0"}) {
			t.Fatalf("unexpected result without distribution metadata: %+v", got)
		}
	})
}
