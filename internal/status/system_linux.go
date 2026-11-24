//go:build linux

package status

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func collectSystem(version string, sectionErrors map[string]string) (*System, error) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return nil, err
	}
	system := &System{ServerVersion: version, UptimeSeconds: uint64(info.Uptime), ProcessArch: runtime.GOARCH}
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		sectionErrors["system:kernel"] = err.Error()
	} else {
		system.KernelRelease = unix.ByteSliceToString(uts.Release[:])
	}
	if err := readOSRelease(os.DirFS("/"), system); err != nil {
		sectionErrors["system:os_release"] = err.Error()
	}
	return system, nil
}

func readOSRelease(root fs.FS, system *System) error {
	data, err := fs.ReadFile(root, "etc/os-release")
	if errors.Is(err, fs.ErrNotExist) {
		data, err = fs.ReadFile(root, "usr/lib/os-release")
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil // A minimal container may not have distribution metadata.
	}
	if err != nil {
		return err
	}
	values := make(map[string]string)
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "ID", "NAME", "PRETTY_NAME", "VERSION", "VERSION_ID":
			if value, ok := osReleaseValue(raw); ok {
				values[key] = value
			}
		}
	}
	system.OSID = values["ID"]
	system.OSName = values["NAME"]
	if system.OSName == "" {
		system.OSName = values["PRETTY_NAME"]
	}
	system.OSVersionID = values["VERSION_ID"]
	system.OSVersion = values["VERSION"]
	if system.OSVersion == "" {
		system.OSVersion = system.OSVersionID
	}
	return nil
}

// os-release assignments support shell quoting but no expansions.
var osReleaseDoubleQuotes = strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`")

func osReleaseValue(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || (value[0] != '\'' && value[0] != '"') {
		return value, true
	}
	quote := value[0]
	if len(value) < 2 || value[len(value)-1] != quote {
		return "", false
	}
	value = value[1 : len(value)-1]
	if quote == '"' {
		value = osReleaseDoubleQuotes.Replace(value)
	}
	return value, true
}
