//go:build linux || darwin

package main

import (
	"testing"

	"github.com/TakashiSasaki/hostmesh-mcp/internal/status"
	"golang.org/x/sys/unix"
)

func checkKernelRelease(t *testing.T, system *status.System) {
	t.Helper()
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		t.Fatal(err)
	}
	want := unix.ByteSliceToString(uts.Release[:])
	if system == nil || system.KernelRelease != want {
		t.Fatalf("system = %+v, want kernel_release %q", system, want)
	}
}
