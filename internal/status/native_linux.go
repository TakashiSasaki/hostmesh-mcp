package status

import (
	"context"

	"golang.org/x/sys/unix"
)

func collectSwap(context.Context) (*Swap, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return nil, err
	}
	total, free := uint64(info.Totalswap)*uint64(info.Unit), uint64(info.Freeswap)*uint64(info.Unit)
	return &Swap{Enabled: total > 0, TotalBytes: total, FreeBytes: free, UsedBytes: total - free}, nil
}

func filesystemUsage(path string) (Filesystem, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return Filesystem{}, err
	}
	size := fs.Frsize
	if size <= 0 {
		size = fs.Bsize
	}
	return Filesystem{Path: path, TotalBytes: fs.Blocks * uint64(size), AvailableBytes: fs.Bavail * uint64(size)}, nil
}
