package status

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/shirou/gopsutil/v4/disk"
	"howett.net/plist"
)

// diskutil includes unmounted partitions and synthesized APFS containers.
type diskRecord struct {
	DeviceIdentifier   string
	VolumeName         string
	Size               *uint64
	Partitions         []diskRecord
	APFSVolumes        []diskRecord
	APFSPhysicalStores []struct{ DeviceIdentifier string }
}

func (r diskRecord) appendDevices(devices *[]BlockDevice, kind, parent string) {
	if r.DeviceIdentifier == "" {
		return
	}
	d := BlockDevice{ID: r.DeviceIdentifier, Name: r.DeviceIdentifier, Kind: kind, ParentID: parent, SizeBytes: r.Size, VolumeName: r.VolumeName}
	if len(r.APFSPhysicalStores) > 0 || r.APFSVolumes != nil {
		d.Kind = "apfs_container"
		for _, store := range r.APFSPhysicalStores {
			if store.DeviceIdentifier != "" {
				d.BackingDeviceIDs = append(d.BackingDeviceIDs, store.DeviceIdentifier)
			}
		}
	}
	*devices = append(*devices, d)
	for _, p := range r.Partitions {
		p.appendDevices(devices, "partition", d.ID)
	}
	for _, v := range r.APFSVolumes {
		v.appendDevices(devices, "apfs_volume", d.ID)
	}
}

func collectStorage(ctx context.Context, sectionErrors map[string]string) (*Storage, error) {
	var storage Storage
	partitions, mountErr := disk.PartitionsWithContext(ctx, true)
	if mountErr != nil {
		sectionErrors["storage:mounts"] = mountErr.Error()
	} else {
		mounts := make([]Mount, 0, len(partitions))
		for _, p := range partitions {
			mounts = append(mounts, Mount{MountPoint: p.Mountpoint, Source: p.Device, FilesystemType: p.Fstype, ReadOnly: slices.Contains(p.Opts, "ro")})
		}
		storage.Mounts = &mounts
	}
	raw, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", "list", "-plist").Output()
	var inventory struct{ AllDisksAndPartitions []diskRecord }
	if err == nil {
		_, err = plist.Unmarshal(raw, &inventory)
		if err == nil && inventory.AllDisksAndPartitions == nil {
			err = fmt.Errorf("diskutil returned no disk inventory")
		}
	}
	if err != nil {
		sectionErrors["storage:block_devices"] = err.Error()
		if mountErr != nil {
			return nil, errors.Join(mountErr, err)
		}
	} else {
		devices := make([]BlockDevice, 0, len(inventory.AllDisksAndPartitions))
		for _, d := range inventory.AllDisksAndPartitions {
			d.appendDevices(&devices, "disk", "")
		}
		storage.BlockDevices = &devices
		// APFS snapshot mount sources may have no row in diskutil's disk list.
		if storage.Mounts != nil {
			known := make(map[string]bool, len(devices))
			for _, d := range devices {
				known[d.ID] = true
			}
			for i := range *storage.Mounts {
				m := &(*storage.Mounts)[i]
				if id, ok := strings.CutPrefix(m.Source, "/dev/"); ok && known[id] {
					m.BlockDeviceID = id
				}
			}
		}
	}
	return &storage, ctx.Err()
}
