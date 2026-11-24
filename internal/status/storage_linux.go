//go:build linux

package status

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func collectStorage(ctx context.Context, sectionErrors map[string]string) (*Storage, error) {
	devices, deviceErr := listBlockDevices(ctx, "/sys/class/block", sectionErrors)
	mounts, mountErr := listMounts(ctx, "/proc/self/mountinfo", devices)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var storage Storage
	if deviceErr != nil {
		sectionErrors["storage:block_devices"] = deviceErr.Error()
	} else {
		storage.BlockDevices = &devices
	}
	if mountErr != nil {
		sectionErrors["storage:mounts"] = mountErr.Error()
	} else {
		storage.Mounts = &mounts
	}
	if storage.Mounts == nil && storage.BlockDevices == nil {
		return nil, errors.New("could not read mounts or block devices")
	}
	return &storage, nil
}

func listMounts(ctx context.Context, path string, devices []BlockDevice) ([]Mount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountInfo(ctx, f, devices)
}

var mountInfoUnescape = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

func parseMountInfo(ctx context.Context, r io.Reader, devices []BlockDevice) ([]Mount, error) {
	knownIDs := make(map[string]bool, len(devices))
	for _, device := range devices {
		knownIDs[device.ID] = true
	}
	mounts := make([]Mount, 0)
	scanner := bufio.NewScanner(r)
	// Overlay mounts can have long option lists.
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields := strings.FieldsFunc(scanner.Text(), func(r rune) bool { return r == ' ' })
		if len(fields) < 10 {
			return nil, errors.New("incomplete mountinfo entry")
		}
		optionalAndFS := fields[6:]
		separator := slices.Index(optionalAndFS, "-")
		if separator < 0 || len(optionalAndFS[separator+1:]) < 3 {
			return nil, errors.New("missing mountinfo filesystem fields")
		}
		fs := optionalAndFS[separator+1:]
		id, err := strconv.Atoi(fields[0])
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid mount ID %q", fields[0])
		}
		mount := Mount{
			MountID:        id,
			MountPoint:     mountInfoUnescape.Replace(fields[4]),
			Root:           mountInfoUnescape.Replace(fields[3]),
			Source:         mountInfoUnescape.Replace(fs[1]),
			FilesystemType: fs[0],
			ReadOnly:       slices.Contains(strings.Split(fields[5], ","), "ro") || slices.Contains(strings.Split(fs[2], ","), "ro"),
		}
		if knownIDs[fields[2]] {
			mount.BlockDeviceID = fields[2]
		} else if strings.HasPrefix(mount.Source, "/dev/") {
			// Some filesystems, such as Btrfs, use a separate mount device ID.
			var stat unix.Stat_t
			if unix.Stat(mount.Source, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFBLK {
				deviceID := fmt.Sprintf("%d:%d", unix.Major(stat.Rdev), unix.Minor(stat.Rdev))
				if knownIDs[deviceID] {
					mount.BlockDeviceID = deviceID
				}
			}
		}
		mounts = append(mounts, mount)
	}
	return mounts, scanner.Err()
}

func listBlockDevices(ctx context.Context, root string, sectionErrors map[string]string) ([]BlockDevice, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	devices := make([]BlockDevice, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		path := filepath.Join(root, name)
		id, err := os.ReadFile(filepath.Join(path, "dev"))
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				sectionErrors["storage:block_devices:"+name] = err.Error()
			}
			continue
		}
		device := BlockDevice{ID: strings.TrimSpace(string(id)), Name: name, Kind: "disk"}
		if _, err := os.Stat(filepath.Join(path, "partition")); err == nil {
			device.Kind = "partition"
			if resolved, err := filepath.EvalSymlinks(path); err == nil {
				device.ParentID = readSysfsText(filepath.Join(filepath.Dir(resolved), "dev"))
			}
		} else if volume := readSysfsText(filepath.Join(path, "dm", "name")); volume != "" {
			device.Kind = "device_mapper"
			device.VolumeName = volume
		} else if _, err := os.Stat(filepath.Join(path, "md")); err == nil {
			device.Kind = "raid"
		} else if strings.HasPrefix(name, "loop") {
			device.Kind = "loop"
		}
		if sectors, err := strconv.ParseUint(readSysfsText(filepath.Join(path, "size")), 10, 64); err == nil && sectors <= math.MaxUint64/512 {
			size := sectors * 512
			device.SizeBytes = &size
		}
		switch value := readSysfsText(filepath.Join(path, "ro")); value {
		case "0", "1":
			readOnly := value == "1"
			device.ReadOnly = &readOnly
		}
		if backing, err := os.ReadDir(filepath.Join(path, "slaves")); err == nil {
			for _, source := range backing {
				if id := readSysfsText(filepath.Join(path, "slaves", source.Name(), "dev")); id != "" {
					device.BackingDeviceIDs = append(device.BackingDeviceIDs, id)
				}
			}
		}
		devices = append(devices, device)
	}
	return devices, nil
}

func readSysfsText(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}
