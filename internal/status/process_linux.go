//go:build linux

package status

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

const processLimit = 100

type processSample struct {
	pid       int32
	startedMS int64
	takenAt   time.Time
	cpu       float64
	name      string
	rss       *uint64
}

func processSnapshot(ctx context.Context, details bool) ([]processSample, int, error) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, 0, err
	}
	samples := make([]processSample, 0, len(pids))
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		p := process.Process{Pid: pid}
		started, err := p.CreateTimeWithContext(ctx)
		if err != nil || started <= 0 {
			continue
		}
		times, err := p.TimesWithContext(ctx)
		if err != nil {
			continue
		}
		row := processSample{pid: pid, startedMS: started, takenAt: time.Now(), cpu: times.User + times.System}
		if details {
			row.name, _ = p.NameWithContext(ctx)
			if m, err := p.MemoryInfoWithContext(ctx); err == nil {
				row.rss = &m.RSS
			}
		}
		// A fresh Process bypasses gopsutil's cached start time.
		fresh := process.Process{Pid: pid}
		if identity, err := fresh.CreateTimeWithContext(ctx); err != nil || identity != started {
			continue
		}
		samples = append(samples, row)
	}
	return samples, len(pids) - len(samples), ctx.Err()
}

func collectProcesses(ctx context.Context) (*ProcessList, error) {
	first, _, err := processSnapshot(ctx, false)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	window := time.Since(start)
	last, skipped, err := processSnapshot(ctx, true)
	if err != nil {
		return nil, err
	}
	if len(last) == 0 && skipped > 0 {
		return nil, errors.New("could not sample visible processes")
	}
	list := summarizeProcesses(first, last, skipped)
	list.SampleWindowMS = window.Milliseconds()
	return list, nil
}

func summarizeProcesses(first, last []processSample, skipped int) *ProcessList {
	initial := make(map[int32]processSample, len(first))
	for _, sample := range first {
		initial[sample.pid] = sample
	}
	items := make([]Process, 0, len(last))
	for _, sample := range last {
		item := Process{PID: sample.pid, Name: sample.name, ResidentBytes: sample.rss}
		if old, ok := initial[sample.pid]; ok && old.startedMS == sample.startedMS && sample.cpu >= old.cpu {
			elapsed := sample.takenAt.Sub(old.takenAt).Seconds()
			if elapsed > 0 {
				percent := (sample.cpu - old.cpu) / elapsed * 100
				item.CPUPercent = &percent
			}
		}
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b Process) int {
		if a.CPUPercent == nil && b.CPUPercent != nil {
			return 1
		}
		if b.CPUPercent == nil && a.CPUPercent != nil {
			return -1
		}
		if a.CPUPercent != nil {
			if order := cmp.Compare(*b.CPUPercent, *a.CPUPercent); order != 0 {
				return order
			}
		}
		return cmp.Compare(a.PID, b.PID)
	})
	return &ProcessList{
		Total: len(items), Skipped: skipped, Truncated: len(items) > processLimit,
		Items: items[:min(len(items), processLimit)],
	}
}
