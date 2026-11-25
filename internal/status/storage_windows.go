package status

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

// A volume can have multiple drive-letter or directory mounts, or none.
func volumePaths(volume *uint16) ([]string, error) {
	buf := make([]uint16, 256)
	for {
		var required uint32
		err := windows.GetVolumePathNamesForVolumeName(volume, &buf[0], uint32(len(buf)), &required)
		if errors.Is(err, windows.ERROR_MORE_DATA) && required > uint32(len(buf)) {
			buf = make([]uint16, required)
			continue
		}
		if err != nil {
			return nil, err
		}
		var paths []string
		for start, i := 0, 0; i < len(buf); i++ {
			if buf[i] != 0 {
				continue
			}
			if start == i {
				break
			}
			paths = append(paths, windows.UTF16ToString(buf[start:i]))
			start = i + 1
		}
		return paths, nil
	}
}

func collectStorage(ctx context.Context, sectionErrors map[string]string) (*Storage, error) {
	mounts := []Mount{}
	devices := []BlockDevice{}
	mounted := map[string]bool{}
	addVolume := func(id string, paths []string) {
		volume, _ := windows.UTF16PtrFromString(id)
		var label, fs [windows.MAX_PATH + 1]uint16
		var flags uint32
		d := BlockDevice{ID: id, Name: id, Kind: "volume"}
		if err := windows.GetVolumeInformation(volume, &label[0], uint32(len(label)), nil, nil, &flags, &fs[0], uint32(len(fs))); err == nil {
			d.VolumeName = windows.UTF16ToString(label[:])
			ro := flags&windows.FILE_READ_ONLY_VOLUME != 0
			d.ReadOnly = &ro
		} else {
			sectionErrors["storage:volume:"+id] = err.Error()
		}
		if usage, err := filesystemUsage(id); err == nil {
			d.SizeBytes = &usage.TotalBytes
		}
		devices = append(devices, d)
		for _, p := range paths {
			mounts = append(mounts, Mount{MountPoint: p, Source: id, BlockDeviceID: id, FilesystemType: windows.UTF16ToString(fs[:]), ReadOnly: d.ReadOnly != nil && *d.ReadOnly})
			mounted[strings.ToUpper(p)] = true
		}
	}
	var name [windows.MAX_PATH + 1]uint16
	h, err := windows.FindFirstVolume(&name[0], uint32(len(name)))
	if err != nil {
		return nil, err
	}
	defer windows.FindVolumeClose(h)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := windows.UTF16ToString(name[:])
		paths, err := volumePaths(&name[0])
		if err != nil {
			sectionErrors["storage:mounts:"+id] = err.Error()
		}
		addVolume(id, paths)
		err = windows.FindNextVolume(h, &name[0], uint32(len(name)))
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			break
		}
		if err != nil {
			sectionErrors["storage:block_devices"] = err.Error()
			break
		}
	}
	// Mapped network drives may be absent from the local volume GUID inventory.
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		sectionErrors["storage:drives"] = err.Error()
	}
	for i := 0; i < 26; i++ {
		path := string(rune('A'+i)) + `:\`
		if drives&(1<<i) != 0 && !mounted[path] {
			addVolume(path, []string{path})
		}
	}
	return &Storage{Mounts: &mounts, BlockDevices: &devices}, nil
}
