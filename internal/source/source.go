// Package source verifies that a configured collection still points at the
// storage it is supposed to.
//
// Every task and the web service run this before touching a single file, which
// is why it lives in its own package: the check must be outside the scan and
// decode packages so that serving a status page does not pull a decoder into
// the process's import graph.
//
// The failure being guarded against is specific and likely on this machine:
// after a power cut the RAID0 array may not have assembled, so
// /mnt/raid0/photos is then just an empty directory on the system SSD. A scan
// that proceeded anyway would mark every photo as missing; a cache write could
// land on the system disk; a thumbnail run would work against the wrong tree.
package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

// Check is the result of verifying one collection.
type Check struct {
	OK         bool
	Root       string
	DataMount  string
	Device     uint64
	RootDevice uint64
	StateDev   uint64
	FSID       string
	IsMount    bool
	Notes      []string
	Fatal      string
}

func (c *Check) note(f string, a ...any) { c.Notes = append(c.Notes, fmt.Sprintf(f, a...)) }

// Verify performs the read-only identity check.
//
// stateDir is where the index and cache live. The media tree must be on a
// different device, which is how "the path fell back onto the system disk" is
// detected without trusting a path string. pinnedFSID is the identity recorded
// on the first successful check; a mismatch refuses to run.
func Verify(col config.Collection, stateDir, pinnedFSID string) Check {
	c := Check{Root: col.Root, DataMount: col.DataMount}

	fi, err := os.Stat(col.Root)
	if err != nil {
		c.Fatal = fmt.Sprintf("collection %q root %s is not accessible: %v", col.Code, col.Root, err)
		return c
	}
	if !fi.IsDir() {
		c.Fatal = fmt.Sprintf("collection %q root %s is not a directory", col.Code, col.Root)
		return c
	}

	rootDev, err := fsutil.DeviceID(col.Root)
	if err != nil {
		c.Fatal = fmt.Sprintf("cannot read the device id of %s: %v", col.Root, err)
		return c
	}
	c.RootDevice = rootDev
	c.Device = rootDev

	if fsid, err := fsutil.FSID(col.Root); err == nil {
		c.FSID = fsid
	} else {
		c.note("filesystem id of %s is unreadable: %v", col.Root, err)
	}

	if col.AllowSharedVolume {
		c.note("allow_shared_volume is set: the check that the media tree is a separate volume from " +
			"albumctl's state is relaxed for this collection, so a missing mount cannot be detected this way")
	}

	if stateDir != "" {
		if sd, err := fsutil.DeviceID(stateDir); err == nil {
			c.StateDev = sd
			if sd == rootDev && !col.AllowSharedVolume {
				c.Fatal = fmt.Sprintf(
					"collection %q root %s is on the same filesystem as the albumctl state directory %s; "+
						"the media tree must be on its own volume so that index and cache writes cannot touch it",
					col.Code, col.Root, stateDir)
				return c
			}
		}
	}

	if sysDev, err := fsutil.DeviceID("/"); err == nil && sysDev == rootDev && !col.AllowSharedVolume {
		c.Fatal = fmt.Sprintf(
			"collection %q root %s resolves to the system filesystem; refusing to run: "+
				"the expected data volume is not mounted",
			col.Code, col.Root)
		return c
	}

	if col.DataMount != "" {
		if !fsutil.IsWithin(col.DataMount, col.Root) && col.Root != col.DataMount {
			c.Fatal = fmt.Sprintf("collection %q root %s is not inside its configured data_mount %s",
				col.Code, col.Root, col.DataMount)
			return c
		}
		mp, err := fsutil.IsMountPoint(col.DataMount)
		if err != nil {
			c.note("cannot determine whether %s is a mount point: %v", col.DataMount, err)
			mp = false
		}
		c.IsMount = mp
		if !mp {
			c.Fatal = fmt.Sprintf(
				"collection %q expects %s to be a mounted volume, but it is not a mount point; "+
					"refusing to run so that a missing mount cannot look like an empty library",
				col.Code, col.DataMount)
			return c
		}
		if dmDev, err := fsutil.DeviceID(col.DataMount); err == nil && dmDev != rootDev {
			c.Fatal = fmt.Sprintf("collection %q root %s is not on the same filesystem as its data_mount %s",
				col.Code, col.Root, col.DataMount)
			return c
		}
	}

	if pinnedFSID != "" && c.FSID != "" && pinnedFSID != c.FSID {
		c.Fatal = fmt.Sprintf(
			"collection %q filesystem id changed from %s to %s; the path now points at different storage. "+
				"Refusing to run. If this is a deliberate replacement, reset the pin with "+
				"`albumctl doctor --reset-fsid %s`.",
			col.Code, pinnedFSID, c.FSID, col.Code)
		return c
	}

	real, err := filepath.EvalSymlinks(col.Root)
	if err != nil {
		c.note("cannot resolve %s: %v", col.Root, err)
	} else if real != "/" {
		c.note("real path is %s", real)
	}
	c.OK = true
	return c
}

// Describe renders the check for a terminal.
func (c Check) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "root=%s\n", c.Root)
	if c.DataMount != "" {
		fmt.Fprintf(&b, "data_mount=%s is_mount_point=%v\n", c.DataMount, c.IsMount)
	}
	fmt.Fprintf(&b, "root_device=%d state_device=%d fsid=%s\n", c.RootDevice, c.StateDev, c.FSID)
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	if c.Fatal != "" {
		fmt.Fprintf(&b, "REFUSED: %s\n", c.Fatal)
	}
	return b.String()
}
