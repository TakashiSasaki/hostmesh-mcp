//go:build linux

package status

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"golang.org/x/sys/unix"
)

type processCollection struct {
	list *ProcessList
	err  error
}

func Native(paths []string, version string) Collector {
	return func(ctx context.Context, node Node, sections []string) (Result, error) {
		result := Result{Node: node, Route: Route{Kind: "local"}, Errors: make(map[string]string)}
		success := 0
		includeClock := false
		var (
			pairedCPU    *CPU
			pairedCPUErr error
			pairedProcs  chan processCollection
		)
		if slices.Contains(sections, "cpu") && slices.Contains(sections, "processes") {
			// The host-wide and per-process samples should cover overlapping time intervals.
			pairedProcs = make(chan processCollection, 1)
			go func() {
				list, err := collectProcesses(ctx, node.ID)
				pairedProcs <- processCollection{list: list, err: err}
			}()
			pairedCPU, pairedCPUErr = collectCPU(ctx)
		}
		for _, section := range sections {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			var err error
			switch section {
			case "system":
				result.Sections.System, err = collectSystem(version, result.Errors)
			case "clock":
				includeClock = true
			case "cpu":
				if pairedProcs != nil {
					result.Sections.CPU, err = pairedCPU, pairedCPUErr
				} else {
					result.Sections.CPU, err = collectCPU(ctx)
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
				if pairedProcs != nil {
					sampled := <-pairedProcs
					result.Sections.Processes, err = sampled.list, sampled.err
				} else {
					result.Sections.Processes, err = collectProcesses(ctx, node.ID)
				}
			case "storage":
				result.Sections.Storage, err = collectStorage(ctx, result.Errors)
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
			return Result{}, &Failure{
				Code: "COLLECTION_FAILED", Message: "all requested sections failed", SectionErrors: result.Errors,
			}
		}
		now := time.Now()
		result.ObservedAt = now.UTC()
		if includeClock {
			zone, offset := now.Zone()
			result.Sections.Clock = &Clock{
				UTCTime: result.ObservedAt, LocalTime: now, TimezoneAbbreviation: zone, UTCOffsetSeconds: offset,
			}
		}
		return result, nil
	}
}

func collectCPU(ctx context.Context) (*CPU, error) {
	start := time.Now()
	values, err := cpu.PercentWithContext(ctx, time.Second, false)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 || values[0] < 0 || values[0] > 100 || math.IsNaN(values[0]) {
		return nil, fmt.Errorf("invalid aggregate CPU sample")
	}
	return &CPU{UsagePercent: values[0], SampleWindowMS: time.Since(start).Milliseconds(), LogicalCPUs: runtime.NumCPU()}, nil
}
