//go:build linux

package status

import (
	"testing"
	"time"
)

func TestProcessRankingAndPIDReuse(t *testing.T) {
	now := time.Now()
	first := []processSample{
		{pid: 10, startTicks: 1, takenAt: now, cpu: 1},
		{pid: 20, startTicks: 2, takenAt: now, cpu: 2},
		{pid: 30, startTicks: 3, takenAt: now, cpu: 0},
	}
	second := []processSample{
		{pid: 10, startTicks: 1, takenAt: now.Add(time.Second), cpu: 1.5},
		{pid: 20, startTicks: 2, takenAt: now.Add(time.Second), cpu: 4.5},
		{pid: 30, startTicks: 4, takenAt: now.Add(time.Second), cpu: 100},
	}
	got := summarizeProcesses(first, second, 1)
	if got.Total != 3 || got.Truncated || got.Skipped != 1 {
		t.Fatalf("unexpected counts: %+v", got)
	}
	if item := got.Items[0]; item.PID != 20 || item.CPUPercent == nil || *item.CPUPercent != 250 {
		t.Fatalf("unexpected highest CPU item: %+v", item)
	}
	if item := got.Items[1]; item.PID != 10 || item.CPUPercent == nil || *item.CPUPercent != 50 {
		t.Fatalf("unexpected second item: %+v", item)
	}
	if item := got.Items[2]; item.PID != 30 || item.CPUPercent != nil {
		t.Fatalf("reused PID must have unknown CPU usage: %+v", item)
	}
}

func TestProcessLimit(t *testing.T) {
	samples := make([]processSample, processLimit+1)
	for i := range samples {
		samples[i].pid = int32(i + 1)
	}
	got := summarizeProcesses(nil, samples, 0)
	if got.Total != processLimit+1 || !got.Truncated || len(got.Items) != processLimit {
		t.Fatalf("unexpected process limit: total=%d, truncated=%t, rows=%d", got.Total, got.Truncated, len(got.Items))
	}
}
