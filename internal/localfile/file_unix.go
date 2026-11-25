//go:build unix

package localfile

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// Lock holds a shared or exclusive lock until f is closed.
func Lock(f *os.File, exclusive, wait bool) error {
	flags := unix.LOCK_SH
	if exclusive {
		flags = unix.LOCK_EX
	}
	if !wait {
		flags |= unix.LOCK_NB
	}
	for {
		err := unix.Flock(int(f.Fd()), flags)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func Replace(src, dst string) error { return os.Rename(src, dst) }

func SyncDir(dir *os.File) error { return dir.Sync() }
