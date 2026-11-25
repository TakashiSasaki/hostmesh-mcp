package status

import (
	"context"
	"time"

	"github.com/shirou/gopsutil/v4/common"
	"github.com/shirou/gopsutil/v4/process"
)

func sampleProcess(ctx context.Context, scope string, pid int32, details bool) (processSample, error) {
	if err := ctx.Err(); err != nil {
		return processSample{}, err
	}
	p := process.Process{Pid: pid}
	started, err := processStartTicks(pid)
	if err != nil {
		return processSample{}, err
	}
	times, err := p.TimesWithContext(ctx)
	if err != nil {
		return processSample{}, err
	}
	row := processSample{pid: pid, startTicks: started, takenAt: time.Now(), cpu: times.User + times.System}
	if details {
		row.name, _ = p.NameWithContext(ctx)
		if m, err := p.MemoryInfoWithContext(ctx); err == nil {
			row.rss = &m.RSS
		}
	}
	current, err := processStartTicks(pid)
	if err != nil {
		return processSample{}, err
	}
	if current != started {
		return processSample{}, errProcessChanged
	}
	if details && scope != "" {
		row.identity = processIdentity(scope, pid, started)
	}
	return row, nil
}

func processSamplingContext(ctx context.Context, nodeID string) (context.Context, string) {
	scope, _ := processIdentityScope(nodeID)
	// Identity and process metrics must come from the same PID namespace.
	return context.WithValue(ctx, common.EnvKey, common.EnvMap{common.HostProcEnvKey: "/proc"}), scope
}
