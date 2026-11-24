package status

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

type Node struct {
	ID        string   `json:"node_id"`
	Hostname  string   `json:"hostname"`
	OS        string   `json:"os"`
	Addresses []string `json:"addresses"`
	Endpoints []string `json:"peer_endpoints,omitempty"`
	Relay     bool     `json:"relay_enabled,omitempty"`
	Revision  uint64   `json:"revision"`
}

type Host struct {
	Node
	Connection string     `json:"connection_state"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}

type Target struct {
	ID       string `json:"node_id,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
}

type Query struct {
	Target   *Target  `json:"target,omitempty"`
	Sections []string `json:"sections,omitempty"`
}

type Route struct {
	Kind    string `json:"kind"`
	RelayID string `json:"relay_node_id,omitempty"`
}

type System struct {
	UptimeSeconds uint64 `json:"uptime_seconds"`
	ProcessArch   string `json:"process_arch"`
}

type CPU struct {
	UsagePercent   float64 `json:"usage_percent"`
	SampleWindowMS int64   `json:"sample_window_ms"`
	LogicalCPUs    int     `json:"logical_cpus_available"`
}

type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type Filesystem struct {
	Path           string `json:"path"`
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type Sections struct {
	System      *System      `json:"system,omitempty"`
	CPU         *CPU         `json:"cpu,omitempty"`
	Memory      *Memory      `json:"memory,omitempty"`
	Filesystems []Filesystem `json:"filesystems,omitempty"`
}

type Result struct {
	Node       Node              `json:"node"`
	ObservedAt time.Time         `json:"observed_at"`
	Route      Route             `json:"route"`
	Sections   Sections          `json:"sections"`
	Errors     map[string]string `json:"section_errors"`
}

type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Candidates []Node `json:"candidates,omitempty"`
}

func (e *Failure) Error() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func Error(code, message string) *Failure {
	return &Failure{Code: code, Message: message}
}

var standard = []string{"system", "cpu", "memory", "filesystems"}

func ValidateSections(sections []string) ([]string, error) {
	if sections == nil {
		return slices.Clone(standard), nil
	}
	if len(sections) == 0 || len(sections) > len(standard) {
		return nil, Error("INVALID_ARGUMENT", "select one or more distinct sections")
	}
	seen := make(map[string]bool)
	for _, s := range sections {
		if !slices.Contains(standard, s) || seen[s] {
			return nil, Error("INVALID_ARGUMENT", fmt.Sprintf("invalid or repeated section %q", s))
		}
		seen[s] = true
	}
	return sections, nil
}

type Collector func(context.Context, Node, []string) (Result, error)
