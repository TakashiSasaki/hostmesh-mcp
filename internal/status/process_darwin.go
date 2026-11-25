package status

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

// Only the Linux write tool needs a public identity. The Darwin samples still
// compare kernel process start times to detect PID reuse between reads.
func processSamplingContext(ctx context.Context, _ string) (context.Context, string) { return ctx, "" }

type darwinProcessAPI struct {
	taskInfo       func(int32, int32, uint64, *process.ProcTaskInfo, int32) int32
	errno          func() *int32
	secondsPerTick float64
}

var processAPI = sync.OnceValues(func() (*darwinProcessAPI, error) {
	var a darwinProcessAPI
	var timebase func(*[2]uint32) int32
	for _, binding := range []struct {
		name string
		fn   any
	}{
		{"proc_pidinfo", &a.taskInfo}, {"__error", &a.errno}, {"mach_timebase_info", &timebase},
	} {
		addr, err := purego.Dlsym(purego.RTLD_DEFAULT, binding.name)
		if err != nil {
			return nil, err
		}
		purego.RegisterFunc(binding.fn, addr)
	}
	var scale [2]uint32
	if timebase(&scale) != 0 || scale[0] == 0 || scale[1] == 0 {
		return nil, fmt.Errorf("invalid Mach timebase")
	}
	a.secondsPerTick = float64(scale[0]) / float64(scale[1]) / 1e9
	return &a, nil
})

func (a *darwinProcessAPI) readTask(pid int32) (process.ProcTaskInfo, error) {
	const procPIDTaskInfo = 4 // PROC_PIDTASKINFO in sys/proc_info.h
	var info process.ProcTaskInfo
	size := int32(unsafe.Sizeof(info))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	errno := a.errno()
	*errno = 0
	if n := a.taskInfo(pid, procPIDTaskInfo, 0, &info, size); n != size {
		if e := unix.Errno(*errno); e != 0 {
			return info, e
		}
		return info, fmt.Errorf("incomplete PROC_PIDTASKINFO for PID %d", pid)
	}
	return info, nil
}

func processInfo(pid int32) (*unix.KinfoProc, error) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	// sysctl returns an empty record for a PID that has exited.
	if err == unix.EIO {
		return nil, unix.ESRCH
	}
	return p, err
}

func sampleProcess(ctx context.Context, _ string, pid int32, details bool) (processSample, error) {
	if err := ctx.Err(); err != nil {
		return processSample{}, err
	}
	api, err := processAPI()
	if err != nil {
		return processSample{}, err
	}
	p, err := processInfo(pid)
	if err != nil {
		return processSample{}, err
	}
	task, err := api.readTask(pid)
	if err != nil {
		return processSample{}, err
	}
	taken := time.Now()
	end, err := processInfo(pid)
	if err != nil {
		return processSample{}, err
	}
	if p.Proc.P_starttime != end.Proc.P_starttime {
		return processSample{}, errProcessChanged
	}
	row := processSample{pid: pid, startTicks: uint64(p.Proc.P_starttime.Nano()), takenAt: taken, cpu: (float64(task.Total_user) + float64(task.Total_system)) * api.secondsPerTick}
	if details {
		row.name = unix.ByteSliceToString(end.Proc.P_comm[:])
		row.rss = &task.Resident_size
	}
	return row, nil
}
