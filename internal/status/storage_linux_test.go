//go:build linux

package status

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMountInfo(t *testing.T) {
	input := `41 33 8:1 /root\040dir /mnt/data\040set A\134040 rw,nosuid shared:7 - ext4 /dev/sda1 rw
42 33 0:34 / /run ro - tmpfs tmpfs rw
43 33 0:35 / /mnt/share rw - nfs4 host:/share ro,vers=4.2
44 33 8:1 / /mnt/data\040set A\134040 rw - ext4 /dev/sda1 rw
`
	mounts, err := parseMountInfo(t.Context(), strings.NewReader(input), []BlockDevice{{ID: "8:1"}})
	if err != nil || len(mounts) != 4 {
		t.Fatalf("mounts = %+v, error = %v", mounts, err)
	}
	first := mounts[0]
	if first.MountID != 41 || first.MountPoint != `/mnt/data set A\040` || first.Root != "/root dir" ||
		first.FilesystemType != "ext4" || first.Source != "/dev/sda1" || first.ReadOnly || first.BlockDeviceID != "8:1" {
		t.Fatalf("ext4 mount = %+v", first)
	}
	if !mounts[1].ReadOnly || !mounts[2].ReadOnly || mounts[1].BlockDeviceID != "" || mounts[2].BlockDeviceID != "" {
		t.Fatalf("tmpfs and nfs4 mounts = %+v", mounts[1:3])
	}
	if mounts[3].MountPoint != first.MountPoint || mounts[3].MountID == first.MountID {
		t.Fatalf("stacked mount = %+v", mounts[3])
	}
}

func TestListBlockDevices(t *testing.T) {
	root := t.TempDir()
	class := filepath.Join(root, "class", "block")
	disk := filepath.Join(root, "devices", "sda")
	part := filepath.Join(disk, "sda1")
	mapped := filepath.Join(root, "devices", "dm-0")

	for path, value := range map[string]string{
		filepath.Join(disk, "dev"):          "8:0\n",
		filepath.Join(part, "dev"):          "8:1\n",
		filepath.Join(part, "size"):         "64\n",
		filepath.Join(part, "partition"):    "1\n",
		filepath.Join(part, "ro"):           "0\n",
		filepath.Join(mapped, "dev"):        "253:0\n",
		filepath.Join(mapped, "dm", "name"): "data-vg\n",
		filepath.Join(mapped, "ro"):         "1\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for path, target := range map[string]string{
		filepath.Join(class, "sda"):             disk,
		filepath.Join(class, "sda1"):            part,
		filepath.Join(class, "dm-0"):            mapped,
		filepath.Join(mapped, "slaves", "sda1"): part,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	devices, err := listBlockDevices(t.Context(), class, make(map[string]string))
	if err != nil || len(devices) != 3 {
		t.Fatalf("devices = %+v, error = %v", devices, err)
	}
	volume, drive, partition := devices[0], devices[1], devices[2]
	if drive.Kind != "disk" || drive.ID != "8:0" ||
		partition.Kind != "partition" || partition.ParentID != drive.ID ||
		partition.SizeBytes == nil || *partition.SizeBytes != 64*512 || partition.ReadOnly == nil || *partition.ReadOnly {
		t.Fatalf("drive and partition = %+v, %+v", drive, partition)
	}
	if volume.Kind != "device_mapper" || volume.VolumeName != "data-vg" || volume.ReadOnly == nil || !*volume.ReadOnly ||
		len(volume.BackingDeviceIDs) != 1 || volume.BackingDeviceIDs[0] != partition.ID {
		t.Fatalf("volume = %+v", volume)
	}
}
