package localfile

import (
	"errors"
	"os"
)

// PopulatePrivateDir creates a new owner-only directory for credentials. An
// existing path is never reused or cleaned up, even when two initializers race.
func PopulatePrivateDir(dir string, populate func() error) error {
	if err := makePrivateDir(dir); err != nil {
		return err
	}
	if err := populate(); err != nil {
		return errors.Join(err, os.RemoveAll(dir))
	}
	return nil
}
