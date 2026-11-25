//go:build linux || darwin

package status

import (
	"context"
	"errors"
	"os"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/unix"
)

func processIDs(ctx context.Context) ([]int32, error) { return process.PidsWithContext(ctx) }

func processAccessError(err error, fallbackCode string) error {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, unix.ESRCH):
		return Error("PROCESS_NOT_FOUND", "process no longer exists")
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
		return Error("FORBIDDEN", "the OS denied access to the process")
	default:
		return Error(fallbackCode, err.Error())
	}
}
