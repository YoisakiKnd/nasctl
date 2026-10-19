// Package storage describes the machine's disks and talks to the storage
// helper, the small root-owned program that mounts, formats and shares on the
// console's behalf. The console itself stays unprivileged: listing disks needs
// only lsblk, and every change goes through the helper's narrow request set.
package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
)

// MountBase is where the helper mounts disks and network shares. Configure it
// as one storage location and every mount appears in the file browser
// without a restart.
const MountBase = "/media/nasctl"

// Disk is one physical disk (or SD card) with its partitions.
type Disk struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Model      string      `json:"model"`
	Serial     string      `json:"serial"`
	Vendor     string      `json:"vendor"`
	Transport  string      `json:"transport"` // usb, sata, nvme, mmc, ...
	Size       int64       `json:"size"`
	Removable  bool        `json:"removable"`
	ReadOnly   bool        `json:"read_only"`
	System     bool        `json:"system"` // holds /, /boot, swap or the like
	Partitions []Partition `json:"partitions"`
}

// Partition is a partition, or the disk itself when it carries a filesystem
// without a partition table.
type Partition struct {
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Size        int64    `json:"size"`
	FSType      string   `json:"fstype"`
	Label       string   `json:"label"`
	UUID        string   `json:"uuid"`
	PartUUID    string   `json:"partuuid,omitempty"`
	Mountpoints []string `json:"mountpoints"`
	Avail       int64    `json:"avail"`
	Used        int64    `json:"used"`
	System      bool     `json:"system"`
	// Managed is true when it is mounted under MountBase by the helper.
	Managed bool `json:"managed"`
}

// LsblkColumns are the columns ParseLsblk reads. MOUNTPOINTS (plural) arrived
// in util-linux 2.37, so older systems are asked for LsblkColumnsOld instead.
// Running lsblk is the helper's job: the web layer must not reach os/exec.
const (
	LsblkColumns    = "NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,LABEL,UUID,PARTUUID,MOUNTPOINTS,MODEL,SERIAL,VENDOR,TRAN,RM,RO,PKNAME,FSAVAIL,FSUSED"
	LsblkColumnsOld = "NAME,KNAME,PATH,TYPE,SIZE,FSTYPE,LABEL,UUID,PARTUUID,MOUNTPOINT,MODEL,SERIAL,VENDOR,TRAN,RM,RO,PKNAME,FSAVAIL,FSUSED"
)

// flex accepts the JSON shapes different lsblk versions print: numbers,
// strings, booleans or null.
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case string(b) == "null":
		*f = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
	default:
		*f = flex(b)
	}
	return nil
}

func (f flex) int() int64  { n, _ := strconv.ParseInt(string(f), 10, 64); return n }
func (f flex) bool() bool  { return f == "true" || f == "1" }
func (f flex) str() string { return strings.TrimSpace(string(f)) }

type node struct {
	Name        flex    `json:"name"`
	KName       flex    `json:"kname"`
	Path        flex    `json:"path"`
	Type        flex    `json:"type"`
	Size        flex    `json:"size"`
	FSType      flex    `json:"fstype"`
	Label       flex    `json:"label"`
	UUID        flex    `json:"uuid"`
	PartUUID    flex    `json:"partuuid"`
	Mountpoint  flex    `json:"mountpoint"`
	Mountpoints []*flex `json:"mountpoints"`
	Model       flex    `json:"model"`
	Serial      flex    `json:"serial"`
	Vendor      flex    `json:"vendor"`
	Tran        flex    `json:"tran"`
	RM          flex    `json:"rm"`
	RO          flex    `json:"ro"`
	FSAvail     flex    `json:"fsavail"`
	FSUsed      flex    `json:"fsused"`
	Children    []node  `json:"children"`
}

func (n node) mounts() []string {
	out := []string{}
	if m := n.Mountpoint.str(); m != "" {
		out = append(out, m)
	}
	for _, m := range n.Mountpoints {
		if m != nil && m.str() != "" {
			out = append(out, m.str())
		}
	}
	return out
}

// systemMount reports mounts that make a disk the system disk.
func systemMount(m string) bool {
	if m == "[SWAP]" || m == "/" {
		return true
	}
	for _, p := range []string{"/boot", "/usr", "/var", "/etc", "/home", "/opt", "/root", "/srv"} {
		if m == p || strings.HasPrefix(m, p+"/") {
			return true
		}
	}
	return false
}

// ParseLsblk turns `lsblk -J -b` output into disks.
func ParseLsblk(data []byte) ([]Disk, error) {
	var doc struct {
		Devices []node `json:"blockdevices"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, errors.New("storage: unreadable lsblk output")
	}
	disks := []Disk{}
	for _, d := range doc.Devices {
		name := d.KName.str()
		if name == "" {
			name = d.Name.str()
		}
		if d.Type.str() != "disk" || strings.HasPrefix(name, "zram") || strings.HasPrefix(name, "ram") ||
			strings.HasPrefix(name, "mtdblock") || strings.Contains(name, "boot") || strings.HasSuffix(name, "rpmb") ||
			d.Size.int() == 0 { // an empty card reader or an unused network block device
			continue
		}
		disk := Disk{Name: name, Path: devPath(d), Model: d.Model.str(), Serial: d.Serial.str(), Vendor: d.Vendor.str(),
			Transport: d.Tran.str(), Size: d.Size.int(), Removable: d.RM.bool(), ReadOnly: d.RO.bool(), Partitions: []Partition{}}
		if disk.Transport == "" && strings.HasPrefix(name, "mmcblk") {
			disk.Transport = "mmc"
		}
		parts := d.Children
		if len(parts) == 0 && d.FSType.str() != "" {
			parts = []node{d} // a filesystem on the whole disk
		}
		var walk func([]node)
		walk = func(nodes []node) {
			for _, p := range nodes {
				part := Partition{Name: p.KName.str(), Path: devPath(p), Size: p.Size.int(), FSType: p.FSType.str(), Label: p.Label.str(),
					UUID: p.UUID.str(), PartUUID: p.PartUUID.str(), Mountpoints: p.mounts(), Avail: p.FSAvail.int(), Used: p.FSUsed.int()}
				if part.Name == "" {
					part.Name = p.Name.str()
				}
				for _, m := range part.Mountpoints {
					if systemMount(m) {
						part.System = true
					}
					if strings.HasPrefix(m, MountBase+"/") {
						part.Managed = true
					}
				}
				disk.System = disk.System || part.System
				if p.Type.str() == "part" || p.Type.str() == "disk" || len(p.Children) == 0 {
					disk.Partitions = append(disk.Partitions, part)
				}
				// LVM, RAID or crypt layers below a partition count toward
				// whether the disk is in use by the system.
				for _, c := range p.Children {
					for _, m := range c.mounts() {
						if systemMount(m) {
							disk.System = true
						}
					}
				}
			}
		}
		walk(parts)
		disks = append(disks, disk)
	}
	sort.SliceStable(disks, func(i, j int) bool {
		if disks[i].System != disks[j].System {
			return disks[i].System
		}
		return disks[i].Name < disks[j].Name
	})
	return disks, nil
}

// MarkSwapFiles marks the partition holding each active swap file as part of
// the system, since lsblk shows only swap partitions. swaps is the content
// of /proc/swaps; a file lies on the partition with the longest mount point
// that contains it.
func MarkSwapFiles(disks []Disk, swaps []byte) {
	for _, line := range strings.Split(string(swaps), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[1] != "file" {
			continue
		}
		file := strings.ReplaceAll(f[0], `\040`, " ")
		di, pi, best := -1, -1, -1
		for i := range disks {
			for j, p := range disks[i].Partitions {
				for _, m := range p.Mountpoints {
					if (m == "/" || file == m || strings.HasPrefix(file, m+"/")) && len(m) > best {
						di, pi, best = i, j, len(m)
					}
				}
			}
		}
		if di >= 0 {
			disks[di].Partitions[pi].System = true
			disks[di].System = true
		}
	}
	sort.SliceStable(disks, func(i, j int) bool { return disks[i].System && !disks[j].System })
}

func devPath(n node) string {
	if p := n.Path.str(); strings.HasPrefix(p, "/dev/") {
		return p
	}
	name := n.KName.str()
	if name == "" {
		name = n.Name.str()
	}
	return path.Join("/dev", name)
}
