// Package sysinfo reads the machine's live status for the desktop widgets:
// CPU, memory, temperature and network throughput. Everything comes from
// /proc and /sys; nothing runs in the background. Rates are deltas between
// two readings, so the sampler keeps only the previous reading.
package sysinfo

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status is one snapshot for the desktop.
type Status struct {
	Hostname string    `json:"hostname"`
	Uptime   int64     `json:"uptime_seconds"`
	CPU      CPU       `json:"cpu"`
	Memory   Memory    `json:"memory"`
	Load     []float64 `json:"load"`
	// TemperatureC is the hottest thermal zone, nil when the board has none.
	TemperatureC *float64    `json:"temperature_c"`
	Network      []Interface `json:"network"`
}

type CPU struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
	Model   string  `json:"model"`
}

type Memory struct {
	TotalBytes     int64   `json:"total_bytes"`
	AvailableBytes int64   `json:"available_bytes"`
	UsedBytes      int64   `json:"used_bytes"`
	Percent        float64 `json:"percent"`
	SwapTotalBytes int64   `json:"swap_total_bytes"`
	SwapUsedBytes  int64   `json:"swap_used_bytes"`
}

type Interface struct {
	Name    string  `json:"name"`
	RxBytes int64   `json:"rx_bytes"`
	TxBytes int64   `json:"tx_bytes"`
	RxRate  float64 `json:"rx_bytes_per_second"`
	TxRate  float64 `json:"tx_bytes_per_second"`
}

// Sampler turns successive readings into rates. It is safe for concurrent use.
type Sampler struct {
	Proc, Sys string

	mu       sync.Mutex
	lastAt   time.Time
	lastCPU  cpuTimes
	lastNet  map[string][2]int64
	model    string
	modelSet bool
}

// New reads the live machine.
func New() *Sampler { return &Sampler{Proc: "/proc", Sys: "/sys"} }

// staleAfter is how old a previous reading may be and still give a fair rate.
// An older one (nobody has looked for a while) is replaced by a short fresh
// measurement, so the first answer after a pause is not a long average.
const staleAfter = 15 * time.Second

// Read returns the status, measuring for a short moment when there is no
// recent previous reading.
func (s *Sampler) Read() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastAt) > staleAfter {
		s.lastCPU, _ = readCPUTimes(s.Proc)
		s.lastNet = readNet(s.Proc)
		s.lastAt = time.Now()
		time.Sleep(250 * time.Millisecond)
	}
	now := time.Now()
	elapsed := now.Sub(s.lastAt).Seconds()
	out := Status{Load: []float64{}, Network: []Interface{}}
	out.Hostname, _ = os.Hostname()
	out.Uptime = readUptime(s.Proc)

	cpu, err := readCPUTimes(s.Proc)
	if err == nil {
		out.CPU.Percent = cpu.busySince(s.lastCPU)
		out.CPU.Cores = cpu.cores
		s.lastCPU = cpu
	}
	if !s.modelSet {
		s.model, s.modelSet = readCPUModel(s.Proc), true
	}
	out.CPU.Model = s.model
	out.Memory = readMemory(s.Proc)
	out.Load = readLoad(s.Proc)
	out.TemperatureC = readTemperature(s.Sys)

	net := readNet(s.Proc)
	names := make([]string, 0, len(net))
	for name := range net {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cur := net[name]
		iface := Interface{Name: name, RxBytes: cur[0], TxBytes: cur[1]}
		if prev, ok := s.lastNet[name]; ok && elapsed > 0 && cur[0] >= prev[0] && cur[1] >= prev[1] {
			iface.RxRate = float64(cur[0]-prev[0]) / elapsed
			iface.TxRate = float64(cur[1]-prev[1]) / elapsed
		}
		out.Network = append(out.Network, iface)
	}
	s.lastNet, s.lastAt = net, now
	return out
}

type cpuTimes struct {
	busy, total uint64
	cores       int
}

func (c cpuTimes) busySince(prev cpuTimes) float64 {
	if c.total <= prev.total || c.busy < prev.busy {
		return 0
	}
	p := float64(c.busy-prev.busy) / float64(c.total-prev.total) * 100
	return float64(int(p*10+0.5)) / 10
}

func readCPUTimes(proc string) (cpuTimes, error) {
	data, err := readSmall(filepath.Join(proc, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	var t cpuTimes
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		switch {
		case fields[0] == "cpu":
			var sum, idle uint64
			for i, f := range fields[1:] {
				v, _ := strconv.ParseUint(f, 10, 64)
				// guest and guest_nice are already counted in user and nice.
				if i >= 8 {
					break
				}
				sum += v
				if i == 3 || i == 4 { // idle, iowait
					idle += v
				}
			}
			t.total, t.busy = sum, sum-idle
		case strings.HasPrefix(fields[0], "cpu"):
			t.cores++
		}
	}
	return t, nil
}

func readCPUModel(proc string) string {
	data, err := readSmall(filepath.Join(proc, "cpuinfo"))
	if err != nil {
		return ""
	}
	for _, key := range []string{"model name", "Hardware", "Processor", "cpu model"} {
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, ":")
			if ok && strings.TrimSpace(k) == key && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func readMemory(proc string) Memory {
	data, err := readSmall(filepath.Join(proc, "meminfo"))
	if err != nil {
		return Memory{}
	}
	kb := map[string]int64{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) > 0 {
			n, _ := strconv.ParseInt(fields[0], 10, 64)
			kb[k] = n
		}
	}
	m := Memory{TotalBytes: kb["MemTotal"] << 10, AvailableBytes: kb["MemAvailable"] << 10,
		SwapTotalBytes: kb["SwapTotal"] << 10, SwapUsedBytes: (kb["SwapTotal"] - kb["SwapFree"]) << 10}
	if m.AvailableBytes == 0 {
		m.AvailableBytes = (kb["MemFree"] + kb["Buffers"] + kb["Cached"]) << 10
	}
	m.UsedBytes = max(0, m.TotalBytes-m.AvailableBytes)
	if m.TotalBytes > 0 {
		m.Percent = float64(int(float64(m.UsedBytes)/float64(m.TotalBytes)*1000+0.5)) / 10
	}
	return m
}

func readLoad(proc string) []float64 {
	data, err := readSmall(filepath.Join(proc, "loadavg"))
	out := []float64{}
	if err != nil {
		return out
	}
	for _, f := range strings.Fields(string(data))[:min(3, len(strings.Fields(string(data))))] {
		v, _ := strconv.ParseFloat(f, 64)
		out = append(out, v)
	}
	return out
}

func readUptime(proc string) int64 {
	data, err := readSmall(filepath.Join(proc, "uptime"))
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return int64(v)
}

// readTemperature returns the hottest plausible thermal zone in °C.
func readTemperature(sys string) *float64 {
	zones, _ := filepath.Glob(filepath.Join(sys, "class", "thermal", "thermal_zone*", "temp"))
	var best *float64
	for _, zone := range zones {
		data, err := readSmall(zone)
		if err != nil {
			continue
		}
		milli, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
		if err != nil {
			continue
		}
		c := milli / 1000
		if c <= -40 || c >= 150 {
			continue
		}
		c = float64(int(c*10+0.5)) / 10
		if best == nil || c > *best {
			best = &c
		}
	}
	return best
}

// virtualPrefixes are interfaces that only mirror traffic already counted on
// a physical one, or carry none worth showing.
var virtualPrefixes = []string{"lo", "docker", "veth", "br-", "virbr", "vnet", "tun", "tap", "wg", "zt", "tailscale", "dummy", "ifb", "sit", "ip6tnl", "gre", "bond-slave"}

func readNet(proc string) map[string][2]int64 {
	out := map[string][2]int64{}
	f, err := os.Open(filepath.Join(proc, "net", "dev"))
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		skip := false
		for _, p := range virtualPrefixes {
			if strings.HasPrefix(name, p) {
				skip = true
				break
			}
		}
		fields := strings.Fields(rest)
		if skip || len(fields) < 9 || len(out) >= 16 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		out[name] = [2]int64{rx, tx}
	}
	return out
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 256<<10))
}
