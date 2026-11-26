//go:build unix

package authority

import "os"

func makePrivateDir(path string) error { return os.Mkdir(path, 0700) }
