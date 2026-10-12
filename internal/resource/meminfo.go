// Package resource reads whole-machine and per-cgroup resource facts and turns
// them into a start / continue / stop verdict.
//
// Three rules shape this package:
//
//  1. MemAvailable is a whole-machine signal, not an albumctl budget. It is
//     never "available minus what we would like to use" and it is never
//     combined with page-cache numbers to invent headroom.
//  2. A single sample at the start of a file is not a limit. The whole point of
//     the supervisor is to keep sampling while a decoder runs.
//  3. RSS of one process is not the cost. We measure the whole descendant tree
//     and, when available, the cgroup, which is the only thing the kernel
//     actually enforces.
package resource

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// MemInfo is the subset of /proc/meminfo this program uses.
//
// Cached/SReclaimable/Buffers are recorded for the report but deliberately not
// used to compute a budget: page cache is reclaimable, not free.
type MemInfo struct {
	MemTotalKB     int64
	MemFreeKB      int64
	MemAvailableKB int64
	BuffersKB      int64
	CachedKB       int64
	SReclaimableKB int64
	ShmemKB        int64
	SwapTotalKB    int64
	SwapFreeKB     int64
	SwapCachedKB   int64
	CommitLimitKB  int64
	CommittedASKB  int64
	DirtyKB        int64
	WritebackKB    int64
	SlabKB         int64
}

// SwapUsedKB is the swap actually in use.
func (m MemInfo) SwapUsedKB() int64 {
	if m.SwapTotalKB <= 0 {
		return 0
	}
	used := m.SwapTotalKB - m.SwapFreeKB
	if used < 0 {
		return 0
	}
	return used
}

// ReadMemInfo parses /proc/meminfo. Values are in kB as the kernel reports.
func ReadMemInfo(path string) (MemInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return MemInfo{}, err
	}
	defer f.Close()

	var m MemInfo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8192), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		val := strings.TrimSpace(line[i+1:])
		val = strings.TrimSuffix(val, " kB")
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.MemTotalKB = n
		case "MemFree":
			m.MemFreeKB = n
		case "MemAvailable":
			m.MemAvailableKB = n
		case "Buffers":
			m.BuffersKB = n
		case "Cached":
			m.CachedKB = n
		case "SReclaimable":
			m.SReclaimableKB = n
		case "Shmem":
			m.ShmemKB = n
		case "SwapTotal":
			m.SwapTotalKB = n
		case "SwapFree":
			m.SwapFreeKB = n
		case "SwapCached":
			m.SwapCachedKB = n
		case "CommitLimit":
			m.CommitLimitKB = n
		case "Committed_AS":
			m.CommittedASKB = n
		case "Dirty":
			m.DirtyKB = n
		case "Writeback":
			m.WritebackKB = n
		case "Slab":
			m.SlabKB = n
		}
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if m.MemTotalKB == 0 {
		return m, fmt.Errorf("meminfo: %s: MemTotal missing or unparsable", path)
	}
	return m, nil
}

// PSI is one /proc/pressure/<resource> file.
//
// total is a monotonically increasing stall counter in microseconds. avg10 is a
// smoothed percentage of wall time during which at least one task stalled
// ("some"). Both are useful: avg10 for a threshold, total for detecting that a
// stall happened at all between two samples.
type PSI struct {
	SomeAvg10  float64
	SomeAvg60  float64
	SomeAvg300 float64
	SomeTotal  int64
	FullAvg10  float64
	FullAvg60  float64
	FullAvg300 float64
	FullTotal  int64
	Present    bool
}

// ReadPSI parses a PSI file. A missing file is not an error: PSI is optional.
func ReadPSI(path string) (PSI, error) {
	var p PSI
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil
		}
		return p, err
	}
	p.Present = true
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		isSome := fields[0] == "some"
		isFull := fields[0] == "full"
		if !isSome && !isFull {
			continue
		}
		for _, f := range fields[1:] {
			kv := strings.SplitN(f, "=", 2)
			if len(kv) != 2 {
				continue
			}
			v, err := strconv.ParseFloat(kv[1], 64)
			if err != nil {
				continue
			}
			switch kv[0] {
			case "avg10":
				if isSome {
					p.SomeAvg10 = v
				} else {
					p.FullAvg10 = v
				}
			case "avg60":
				if isSome {
					p.SomeAvg60 = v
				} else {
					p.FullAvg60 = v
				}
			case "avg300":
				if isSome {
					p.SomeAvg300 = v
				} else {
					p.FullAvg300 = v
				}
			case "total":
				if isSome {
					p.SomeTotal = int64(v)
				} else {
					p.FullTotal = int64(v)
				}
			}
		}
	}
	return p, nil
}

// LoadAvg reports the 1/5/15 minute load average. Used for reporting only; load
// average on its own must not gate media work because it counts uninterruptible
// I/O wait that the guard already tracks via PSI.
type LoadAvg struct{ One, Five, Fifteen float64 }

// ReadLoadAvg parses /proc/loadavg.
func ReadLoadAvg(path string) (LoadAvg, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LoadAvg{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return LoadAvg{}, fmt.Errorf("loadavg: unexpected content %q", string(data))
	}
	var l LoadAvg
	l.One, _ = strconv.ParseFloat(fields[0], 64)
	l.Five, _ = strconv.ParseFloat(fields[1], 64)
	l.Fifteen, _ = strconv.ParseFloat(fields[2], 64)
	return l, nil
}

// Cgroup stats for cgroup v2. All fields come from the memory controller.
type Cgroup struct {
	FS          string // filesystem path of the cgroup directory
	Current     int64  // memory.current
	Peak        int64  // memory.peak, 0 if unavailable
	SwapCur     int64  // memory.swap.current
	High        int64  // memory.high, -1 when unset ("max")
	Max         int64  // memory.max, -1 when unset
	Low         int64
	LimitsKnown bool

	// memory.events counters (monotonic).
	EventsLow          int64
	EventsHigh         int64
	EventsMax          int64
	EventsOOM          int64
	EventsOOMKill      int64
	EventsOOMGroupKill int64

	// memory.stat, the interesting few.
	Anon          int64
	File          int64
	KernelStack   int64
	Slab          int64
	Sock          int64
	Shmem         int64
	FileMapped    int64
	InactiveAnon  int64
	ActiveAnon    int64
	InactiveFile  int64
	ActiveFile    int64
	Pgscan        int64
	Pgsteal       int64
	PgscanDirect  int64
	PgstealDirect int64
	Refault       int64

	// memory.pressure, when present.
	Pressure PSI

	Present bool
}

const cgroupUnset = -1

// ReadCgroup reads the memory controller files under dir.
//
// A directory that does not exist yields Present=false and no error: albumctl
// must work in a plain shell, a container without delegation, or a systemd unit
// without Delegate=yes, and report honestly that cgroup accounting is absent.
func ReadCgroup(dir string) (Cgroup, error) {
	c := Cgroup{FS: dir, High: cgroupUnset, Max: cgroupUnset, Low: cgroupUnset}
	st, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	if !st.IsDir() {
		return c, fmt.Errorf("cgroup: %s is not a directory", dir)
	}
	c.Present = true

	if v, ok := readInt63(dir + "/memory.current"); ok {
		c.Current = v
	}
	if v, ok := readInt63(dir + "/memory.peak"); ok {
		c.Peak = v
	}
	if v, ok := readInt63(dir + "/memory.swap.current"); ok {
		c.SwapCur = v
	}
	if v, ok := readInt63(dir + "/memory.high"); ok {
		c.High = v
		c.LimitsKnown = true
	}
	if v, ok := readInt63(dir + "/memory.max"); ok {
		c.Max = v
		c.LimitsKnown = true
	}
	if v, ok := readInt63(dir + "/memory.low"); ok {
		c.Low = v
	}

	if kvs, err := readKeyVals(dir + "/memory.events"); err == nil {
		c.EventsLow = kvs["low"]
		c.EventsHigh = kvs["high"]
		c.EventsMax = kvs["max"]
		c.EventsOOM = kvs["oom"]
		c.EventsOOMKill = kvs["oom_kill"]
		c.EventsOOMGroupKill = kvs["oom_group_kill"]
	}
	if kvs, err := readKeyVals(dir + "/memory.stat"); err == nil {
		c.Anon = kvs["anon"]
		c.File = kvs["file"]
		c.KernelStack = kvs["kernel_stack"]
		c.Slab = kvs["slab"]
		c.Sock = kvs["sock"]
		c.Shmem = kvs["shmem"]
		c.FileMapped = kvs["file_mapped"]
		c.InactiveAnon = kvs["inactive_anon"]
		c.ActiveAnon = kvs["active_anon"]
		c.InactiveFile = kvs["inactive_file"]
		c.ActiveFile = kvs["active_file"]
		c.Pgscan = kvs["pgscan"]
		c.Pgsteal = kvs["pgsteal"]
		c.PgscanDirect = kvs["pgscan_direct"]
		c.PgstealDirect = kvs["pgsteal_direct"]
		c.Refault = kvs["workingset_refault"]
	}
	if p, err := ReadPSI(dir + "/memory.pressure"); err == nil {
		c.Pressure = p
	}
	return c, nil
}

// LimitDescription renders the effective memory limit for humans.
func (c Cgroup) LimitDescription() string {
	if !c.Present {
		return "cgroup accounting unavailable"
	}
	hd := "unset"
	if c.High != cgroupUnset && c.High > 0 {
		hd = fmt.Sprintf("%.1f MiB", float64(c.High)/(1<<20))
	} else if c.High == cgroupUnset || c.High < 0 {
		hd = "max"
	}
	mx := "max"
	if c.Max > 0 {
		mx = fmt.Sprintf("%.1f MiB", float64(c.Max)/(1<<20))
	}
	return fmt.Sprintf("memory.current=%.1f MiB memory.high=%s memory.max=%s",
		float64(c.Current)/(1<<20), hd, mx)
}

func readInt63(path string) (int64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return cgroupUnset, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readKeyVals(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 8192), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[fields[0]] = n
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// Uptime returns the system uptime, used only for reporting.
func Uptime(procRoot string) (time.Duration, error) {
	data, err := os.ReadFile(procRoot + "/uptime")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("uptime: empty")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}
