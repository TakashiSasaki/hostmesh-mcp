//go:build unix

package localfile

import "os"

func makePrivateDir(path string) error { return os.Mkdir(path, 0700) }
