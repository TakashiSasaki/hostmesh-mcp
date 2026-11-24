//go:build linux

package status

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type ProcessKillRequest struct {
	PID      int32  `json:"pid" jsonschema:"PID of one local process"`
	Identity string `json:"process_identity" jsonschema:"Identity returned for the PID in a local processes snapshot"`
	Signal   string `json:"signal,omitempty" jsonschema:"TERM (default) or KILL"`
}

type ProcessKillResult struct {
	PID        int32  `json:"pid"`
	Signal     string `json:"signal"`
	SignalSent bool   `json:"signal_sent"`
}

func processStartTicks(pid int32) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The parenthesized process name may itself contain spaces and parentheses.
	text := string(data)
	end := strings.LastIndexByte(text, ')')
	if end < 0 {
		return 0, fmt.Errorf("malformed process stat")
	}
	fields := strings.Fields(text[end+1:])
	const startTimeIndex = 19 // /proc/pid/stat field 22, after pid and comm
	if len(fields) <= startTimeIndex {
		return 0, fmt.Errorf("missing process start time")
	}
	return strconv.ParseUint(fields[startTimeIndex], 10, 64)
}

func processIdentityScope(nodeID string) (string, error) {
	self, err := os.Readlink("/proc/self")
	if err != nil || self != strconv.Itoa(os.Getpid()) {
		return "", fmt.Errorf("procfs must reflect the server's PID namespace")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	namespace, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return "", err
	}
	return nodeID + "\x00" + strings.TrimSpace(string(bootID)) + "\x00" + namespace, nil
}

func processIdentity(scope string, pid int32, ticks uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d", scope, pid, ticks)))
	return hex.EncodeToString(sum[:])
}

func KillProcess(ctx context.Context, nodeID string, req ProcessKillRequest) (ProcessKillResult, error) {
	if req.PID <= 1 || int(req.PID) == os.Getpid() {
		return ProcessKillResult{}, Error("INVALID_ARGUMENT", "choose one process other than PID 1 or the MCP server itself")
	}
	if decoded, err := hex.DecodeString(req.Identity); err != nil || len(decoded) != sha256.Size {
		return ProcessKillResult{}, Error("INVALID_ARGUMENT", "copy process_identity from the local process list")
	}
	name := strings.ToUpper(req.Signal)
	var signal unix.Signal
	switch name {
	case "", "TERM":
		name, signal = "TERM", unix.SIGTERM
	case "KILL":
		signal = unix.SIGKILL
	default:
		return ProcessKillResult{}, Error("INVALID_ARGUMENT", "signal must be TERM or KILL")
	}
	if err := ctx.Err(); err != nil {
		return ProcessKillResult{}, err
	}
	scope, err := processIdentityScope(nodeID)
	if err != nil {
		return ProcessKillResult{}, Error("PROCESS_IDENTITY_UNAVAILABLE", err.Error())
	}

	pidfd, err := unix.PidfdOpen(int(req.PID), 0)
	if err != nil {
		return ProcessKillResult{}, processSignalError(err)
	}
	defer unix.Close(pidfd)
	ticks, err := processStartTicks(req.PID)
	if err != nil {
		return ProcessKillResult{}, processSignalError(err)
	}
	if processIdentity(scope, req.PID, ticks) != req.Identity {
		return ProcessKillResult{}, Error("PROCESS_IDENTITY_MISMATCH", "process identity changed or belongs to a different node")
	}
	if err := ctx.Err(); err != nil {
		return ProcessKillResult{}, err
	}
	if err := unix.PidfdSendSignal(pidfd, signal, nil, 0); err != nil {
		return ProcessKillResult{}, processSignalError(err)
	}
	return ProcessKillResult{PID: req.PID, Signal: name, SignalSent: true}, nil
}

func processSignalError(err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, unix.ESRCH):
		return Error("PROCESS_NOT_FOUND", "process no longer exists")
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return Error("FORBIDDEN", "the OS denied access to the process")
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		return Error("UNSUPPORTED", "the OS does not support pidfd process signaling")
	default:
		return Error("SIGNAL_FAILED", err.Error())
	}
}
