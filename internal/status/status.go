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
	ServerVersion string `json:"server_version,omitempty"`
	UptimeSeconds uint64 `json:"uptime_seconds"`
	ProcessArch   string `json:"process_arch"`
}

type Clock struct {
	UTCTime              time.Time `json:"utc_time"`
	LocalTime            time.Time `json:"local_time"`
	TimezoneAbbreviation string    `json:"timezone_abbreviation"`
	UTCOffsetSeconds     int       `json:"utc_offset_seconds"`
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

type Mount struct {
	MountID        int    `json:"mount_id"`
	MountPoint     string `json:"mount_point"`
	Root           string `json:"root"`
	Source         string `json:"source"`
	FilesystemType string `json:"filesystem_type"`
	ReadOnly       bool   `json:"read_only"`
	BlockDeviceID  string `json:"block_device_id,omitempty"`
}

type BlockDevice struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Kind             string   `json:"kind"`
	VolumeName       string   `json:"volume_name,omitempty"`
	SizeBytes        *uint64  `json:"size_bytes,omitempty"`
	ReadOnly         *bool    `json:"read_only,omitempty"`
	ParentID         string   `json:"parent_id,omitempty"`
	BackingDeviceIDs []string `json:"backing_device_ids,omitempty"`
}

type Storage struct {
	Mounts       *[]Mount       `json:"mounts,omitempty"`
	BlockDevices *[]BlockDevice `json:"block_devices,omitempty"`
}

type Swap struct {
	Enabled    bool   `json:"enabled"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

type Interface struct {
	Index     int      `json:"index"`
	Name      string   `json:"name"`
	Up        bool     `json:"up"`
	Running   bool     `json:"running"`
	Loopback  bool     `json:"loopback"`
	MTU       int      `json:"mtu"`
	Addresses []string `json:"addresses"`
}

type Process struct {
	Identity      string   `json:"process_identity,omitempty"`
	PID           int32    `json:"pid"`
	Name          string   `json:"name"`
	CPUPercent    *float64 `json:"cpu_percent"`
	ResidentBytes *uint64  `json:"resident_bytes"`
}

type ProcessList struct {
	SampleWindowMS int64     `json:"sample_window_ms"`
	Total          int       `json:"total"`
	Skipped        int       `json:"skipped"`
	Truncated      bool      `json:"truncated"`
	Items          []Process `json:"items"`
}

type Sections struct {
	Storage     *Storage     `json:"storage,omitempty"`
	Swap        *Swap        `json:"swap,omitempty"`
	Network     []Interface  `json:"network,omitempty"`
	Processes   *ProcessList `json:"processes,omitempty"`
	System      *System      `json:"system,omitempty"`
	Clock       *Clock       `json:"clock,omitempty"`
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

func SupportedSections() []string {
	return slices.Concat(standard, []string{"swap", "network", "processes", "storage", "clock"})
}

func ValidateSections(sections []string) ([]string, error) {
	if sections == nil {
		return slices.Clone(standard), nil
	}
	supported := SupportedSections()
	if len(sections) == 0 || len(sections) > len(supported) {
		return nil, Error("INVALID_ARGUMENT", "select one or more distinct sections")
	}
	seen := make(map[string]bool)
	for _, s := range sections {
		if !slices.Contains(supported, s) || seen[s] {
			return nil, Error("INVALID_ARGUMENT", fmt.Sprintf("invalid or repeated section %q", s))
		}
		seen[s] = true
	}
	return sections, nil
}

type Collector func(context.Context, Node, []string) (Result, error)
