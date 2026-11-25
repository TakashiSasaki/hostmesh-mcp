package status

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func collectSystem(version string, _ map[string]string) (*System, error) {
	v := windows.RtlGetVersion()
	build := fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	name := "Windows"
	if v.ProductType != 1 {
		name = "Windows Server"
	}
	s := &System{OSID: "windows", OSName: name, OSVersion: build, OSVersionID: build, KernelRelease: build, ProcessArch: runtime.GOARCH, ServerVersion: version, UptimeSeconds: uint64(windows.DurationSinceBoot() / time.Second)}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err == nil {
		defer k.Close()
		if release, _, err := k.GetStringValue("DisplayVersion"); err == nil && release != "" {
			s.OSVersion = release
		}
	}
	return s, nil
}

func collectSwap(ctx context.Context) (*Swap, error) {
	files, err := mem.SwapDevicesWithContext(ctx)
	if err != nil {
		return nil, err
	}
	s := &Swap{Enabled: len(files) > 0}
	for _, f := range files {
		p := PageFile{Name: f.Name, UsedBytes: f.UsedBytes, TotalBytes: f.UsedBytes + f.FreeBytes}
		s.PageFiles = append(s.PageFiles, p)
		s.TotalBytes += p.TotalBytes
		s.UsedBytes += f.UsedBytes
		s.FreeBytes += f.FreeBytes
	}
	return s, nil
}

func filesystemUsage(path string) (Filesystem, error) {
	ptr, err := windows.UTF16PtrFromString(path)
	fs := Filesystem{Path: path}
	if err != nil {
		return fs, err
	}
	err = windows.GetDiskFreeSpaceEx(ptr, &fs.AvailableBytes, &fs.TotalBytes, nil)
	return fs, err
}
