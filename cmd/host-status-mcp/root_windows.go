package main

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func defaultFilesystemRoot() string {
	if dir, err := windows.GetSystemDirectory(); err == nil {
		return filepath.VolumeName(dir) + `\`
	}
	return `C:\`
}
