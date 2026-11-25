package status

import (
	"context"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"
)

func collectSystem(version string, sectionErrors map[string]string) (*System, error) {
	boot, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return nil, err
	}
	s := &System{ServerVersion: version, ProcessArch: runtime.GOARCH, OSID: "macos", OSName: "macOS", UptimeSeconds: uint64(max(0, time.Now().Unix()-boot.Sec))}
	s.OSVersion, err = unix.Sysctl("kern.osproductversion")
	if err != nil {
		sectionErrors["system:os_version"] = err.Error()
	}
	s.OSVersionID = s.OSVersion
	s.KernelRelease, err = unix.Sysctl("kern.osrelease")
	if err != nil {
		sectionErrors["system:kernel"] = err.Error()
	}
	return s, nil
}

func collectSwap(ctx context.Context) (*Swap, error) {
	s, err := mem.SwapMemoryWithContext(ctx)
	if err != nil {
		return nil, err
	}
	return &Swap{Enabled: s.Total > 0, TotalBytes: s.Total, FreeBytes: s.Free, UsedBytes: s.Used}, nil
}

func filesystemUsage(path string) (Filesystem, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return Filesystem{}, err
	}
	return Filesystem{Path: path, TotalBytes: fs.Blocks * uint64(fs.Bsize), AvailableBytes: fs.Bavail * uint64(fs.Bsize)}, nil
}
