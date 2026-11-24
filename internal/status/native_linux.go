//go:build linux

package status

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"
)

func Native(paths []string, version string) Collector {
	return func(ctx context.Context, node Node, sections []string) (Result, error) {
		result := Result{Node: node, Route: Route{Kind: "local"}, Errors: make(map[string]string)}
		success := 0
		for _, section := range sections {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			var err error
			switch section {
			case "system":
				var info unix.Sysinfo_t
				err = unix.Sysinfo(&info)
				if err == nil {
					result.Sections.System = &System{ServerVersion: version, UptimeSeconds: uint64(info.Uptime), ProcessArch: runtime.GOARCH}
				}
			case "cpu":
				start := time.Now()
				var values []float64
				values, err = cpu.PercentWithContext(ctx, time.Second, false)
				if err == nil {
					if len(values) != 1 || values[0] < 0 || values[0] > 100 || math.IsNaN(values[0]) {
						err = fmt.Errorf("invalid aggregate CPU sample")
					} else {
						result.Sections.CPU = &CPU{UsagePercent: values[0], SampleWindowMS: time.Since(start).Milliseconds(), LogicalCPUs: runtime.NumCPU()}
					}
				}
			case "memory":
				var value *mem.VirtualMemoryStat
				value, err = mem.VirtualMemoryWithContext(ctx)
				if err == nil {
					result.Sections.Memory = &Memory{TotalBytes: value.Total, AvailableBytes: value.Available}
				}
			case "swap":
				var info unix.Sysinfo_t
				err = unix.Sysinfo(&info)
				if err == nil {
					total := uint64(info.Totalswap) * uint64(info.Unit)
					free := uint64(info.Freeswap) * uint64(info.Unit)
					result.Sections.Swap = &Swap{Enabled: total > 0, TotalBytes: total, FreeBytes: free, UsedBytes: total - free}
				}
			case "network":
				result.Sections.Network, err = networkInterfaces(result.Errors)
			case "processes":
				result.Sections.Processes, err = collectProcesses(ctx)
			case "filesystems":
				for _, path := range paths {
					if err := ctx.Err(); err != nil {
						return Result{}, err
					}
					var fs unix.Statfs_t
					if statErr := unix.Statfs(path, &fs); statErr != nil {
						result.Errors["filesystems:"+path] = statErr.Error()
						continue
					}
					size := fs.Frsize
					if size <= 0 {
						size = fs.Bsize
					}
					result.Sections.Filesystems = append(result.Sections.Filesystems, Filesystem{Path: path, TotalBytes: fs.Blocks * uint64(size), AvailableBytes: fs.Bavail * uint64(size)})
				}
				if len(result.Sections.Filesystems) == 0 {
					err = fmt.Errorf("no configured filesystem could be measured")
				}
			}
			if err != nil {
				result.Errors[section] = err.Error()
			} else {
				success++
			}
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if success == 0 {
			return Result{}, Error("COLLECTION_FAILED", "all requested sections failed")
		}
		result.ObservedAt = time.Now().UTC()
		return result, nil
	}
}
