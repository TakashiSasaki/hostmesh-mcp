//go:build linux || darwin || windows

package status

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"
)

const processLimit = 100

type processSample struct {
	pid        int32
	startTicks uint64
	identity   string
	takenAt    time.Time
	cpu        float64
	name       string
	rss        *uint64
}

var errProcessChanged = errors.New("process changed during sampling")

func processSnapshot(ctx context.Context, scope string, details bool) ([]processSample, int, error) {
	pids, err := processIDs(ctx)
	if err != nil {
		return nil, 0, err
	}
	samples := make([]processSample, 0, len(pids))
	for _, pid := range pids {
		row, err := sampleProcess(ctx, scope, pid, details)
		if err == nil {
			samples = append(samples, row)
		} else if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
	}
	return samples, len(pids) - len(samples), ctx.Err()
}

func waitProcessSample(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return time.Since(start), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func ReadProcess(ctx context.Context, nodeID string, pid int32) (Process, error) {
	if pid <= 0 {
		return Process{}, Error("INVALID_ARGUMENT", "pid must be positive")
	}
	ctx, scope := processSamplingContext(ctx, nodeID)
	first, err := sampleProcess(ctx, scope, pid, false)
	if err != nil {
		return Process{}, processReadError(err)
	}
	if _, err := waitProcessSample(ctx); err != nil {
		return Process{}, err
	}
	last, err := sampleProcess(ctx, scope, pid, true)
	if err != nil {
		return Process{}, processReadError(err)
	}
	if first.startTicks != last.startTicks {
		return Process{}, processReadError(errProcessChanged)
	}
	return processRow(last, first), nil
}

func processReadError(err error) error {
	switch {
	case errors.Is(err, errProcessChanged):
		return Error("PROCESS_IDENTITY_MISMATCH", err.Error())
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return processAccessError(err, "PROCESS_READ_FAILED")
	}
}

func collectProcesses(ctx context.Context, nodeID string) (*ProcessList, error) {
	ctx, scope := processSamplingContext(ctx, nodeID)
	first, _, err := processSnapshot(ctx, scope, false)
	if err != nil {
		return nil, err
	}
	window, err := waitProcessSample(ctx)
	if err != nil {
		return nil, err
	}
	last, skipped, err := processSnapshot(ctx, scope, true)
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
		items = append(items, processRow(sample, initial[sample.pid]))
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

func processRow(sample, previous processSample) Process {
	item := Process{Identity: sample.identity, PID: sample.pid, Name: sample.name, ResidentBytes: sample.rss}
	if !previous.takenAt.IsZero() && previous.startTicks == sample.startTicks && sample.cpu >= previous.cpu {
		if elapsed := sample.takenAt.Sub(previous.takenAt).Seconds(); elapsed > 0 {
			percent := (sample.cpu - previous.cpu) / elapsed * 100
			item.CPUPercent = &percent
		}
	}
	return item
}
