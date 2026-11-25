package status

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func processSamplingContext(ctx context.Context, _ string) (context.Context, string) { return ctx, "" }

func processIDs(ctx context.Context) ([]int32, error) {
	for size := 512; ; size *= 2 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ids := make([]uint32, size)
		var n uint32
		if err := windows.EnumProcesses(ids, &n); err != nil {
			return nil, err
		}
		if int(n/4) == len(ids) {
			continue
		}
		out := make([]int32, 0, n/4)
		for _, pid := range ids[:n/4] {
			if pid != 0 {
				out = append(out, int32(pid))
			}
		}
		return out, nil
	}
}

var getProcessMemoryInfo = windows.NewLazySystemDLL("kernel32.dll").NewProc("K32GetProcessMemoryInfo")

// PROCESS_MEMORY_COUNTERS uses SIZE_T for the eight fields after PageFaultCount.
type processMemoryCounters struct {
	Size, PageFaultCount       uint32
	PeakWorkingSet, WorkingSet uintptr
	Other                      [6]uintptr
}

func filetimeTicks(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

func sampleProcess(ctx context.Context, _ string, pid int32, details bool) (processSample, error) {
	if err := ctx.Err(); err != nil {
		return processSample{}, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return processSample{}, err
	}
	defer windows.CloseHandle(h)
	var start, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &start, &exit, &kernel, &user); err != nil {
		return processSample{}, err
	}
	row := processSample{pid: pid, startTicks: filetimeTicks(start), takenAt: time.Now(), cpu: float64(filetimeTicks(kernel)+filetimeTicks(user)) / 1e7}
	if details {
		var name [windows.MAX_PATH]uint16
		n := uint32(len(name))
		err := windows.QueryFullProcessImageName(h, 0, &name[0], &n)
		if err == nil {
			row.name = filepath.Base(windows.UTF16ToString(name[:n]))
		} else if errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			long := make([]uint16, 32768)
			n = uint32(len(long))
			if windows.QueryFullProcessImageName(h, 0, &long[0], &n) == nil {
				row.name = filepath.Base(windows.UTF16ToString(long[:n]))
			}
		}
		var m processMemoryCounters
		m.Size = uint32(unsafe.Sizeof(m))
		if ok, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.Size)); ok != 0 {
			rss := uint64(m.WorkingSet)
			row.rss = &rss
		}
	}
	return row, nil
}

func processAccessError(err error, fallback string) error {
	switch {
	case errors.Is(err, windows.ERROR_INVALID_PARAMETER), errors.Is(err, os.ErrNotExist):
		return Error("PROCESS_NOT_FOUND", "process no longer exists")
	case errors.Is(err, windows.ERROR_ACCESS_DENIED):
		return Error("FORBIDDEN", "the OS denied access to the process")
	default:
		return Error(fallback, err.Error())
	}
}
