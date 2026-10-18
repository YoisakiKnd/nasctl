package sysinfo

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadComputesRatesFromSuccessiveReadings(t *testing.T) {
	dir := t.TempDir()
	proc, sys := filepath.Join(dir, "proc"), filepath.Join(dir, "sys")
	stat := func(user, idle int) string {
		return "cpu  " + itoa(user) + " 0 0 " + itoa(idle) + " 0 0 0 0 0 0\ncpu0 1 0 0 1 0 0 0 0 0 0\ncpu1 1 0 0 1 0 0 0 0 0 0\n"
	}
	netdev := func(rx, tx int) string {
		return "Inter-|   Receive\n face |bytes\n    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n  eth0: " + itoa(rx) + " 0 0 0 0 0 0 0 " + itoa(tx) + " 0 0 0 0 0 0 0\ndocker0: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n"
	}
	write(t, filepath.Join(proc, "stat"), stat(100, 900))
	write(t, filepath.Join(proc, "net", "dev"), netdev(1000, 500))
	write(t, filepath.Join(proc, "meminfo"), "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 250 kB\nSwapTotal: 100 kB\nSwapFree: 40 kB\n")
	write(t, filepath.Join(proc, "loadavg"), "0.50 0.25 0.10 1/100 42\n")
	write(t, filepath.Join(proc, "uptime"), "3600.5 100.0\n")
	write(t, filepath.Join(proc, "cpuinfo"), "processor : 0\nHardware : Amlogic S805\n")
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone0", "temp"), "48500\n")
	write(t, filepath.Join(sys, "class", "thermal", "thermal_zone1", "temp"), "52000\n")

	s := &Sampler{Proc: proc, Sys: sys}
	s.Read() // the first reading measures briefly against itself
	write(t, filepath.Join(proc, "stat"), stat(150, 950))
	write(t, filepath.Join(proc, "net", "dev"), netdev(3000, 1500))
	s.lastAt = time.Now().Add(-2 * time.Second)
	got := s.Read()
	if got.CPU.Percent != 50 || got.CPU.Cores != 2 || got.CPU.Model != "Amlogic S805" {
		t.Fatalf("cpu %+v", got.CPU)
	}
	if got.Memory.TotalBytes != 1000<<10 || got.Memory.UsedBytes != 750<<10 || got.Memory.Percent != 75 || got.Memory.SwapUsedBytes != 60<<10 {
		t.Fatalf("memory %+v", got.Memory)
	}
	if len(got.Network) != 1 || got.Network[0].Name != "eth0" || got.Network[0].RxRate < 900 || got.Network[0].RxRate > 1100 {
		t.Fatalf("network %+v", got.Network)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 52 || got.Uptime != 3600 || len(got.Load) != 3 {
		t.Fatalf("temp %v uptime %d load %v", got.TemperatureC, got.Uptime, got.Load)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
