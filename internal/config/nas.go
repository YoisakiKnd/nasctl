package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const NASSettingsFile = "nasctl-settings.json"

// NASOverrides is private state, not an API response. Secrets are stored at
// 0600 and never returned by the settings view. Album processing snapshots do
// not automatically load it: their sanitized, bounded config remains isolated.
type NASOverrides struct {
	Version     int          `json:"version"`
	Storage     []HubStorage `json:"storage"`
	Files       FileLimits   `json:"files"`
	Jobs        JobLimits    `json:"jobs"`
	Anime       Anime        `json:"anime"`
	GuardMemory GuardMemory  `json:"guard_memory"`
}
type GuardMemory struct {
	StartKB      int64 `json:"start_kb"`
	WarnKB       int64 `json:"warn_kb"`
	StopKB       int64 `json:"stop_kb"`
	WorkerHighMB int64 `json:"worker_high_mb"`
	WorkerMaxMB  int64 `json:"worker_max_mb"`
}

func MemoryOptions(c *Config) GuardMemory {
	return GuardMemory{c.Guard.StartMemAvailableKB, c.Guard.WarnMemAvailableKB, c.Guard.StopMemAvailableKB, c.Guard.WorkerMemHighMB, c.Guard.WorkerMemMaxMB}
}
func Options(c *Config) NASOverrides {
	return NASOverrides{1, c.Hub.Storage, c.Hub.Files, c.Hub.Jobs, c.Hub.Anime, MemoryOptions(c)}
}
func ApplyNASOptions(c *Config, o NASOverrides) error {
	if o.Version != 1 || len(o.Storage) > 64 || o.GuardMemory.StartKB <= 0 || o.GuardMemory.StartKB > 1<<40 || o.GuardMemory.WorkerMaxMB > 1<<20 {
		return errors.New("invalid NAS settings bounds")
	}
	if o.GuardMemory.WarnKB <= 0 || o.GuardMemory.StopKB <= 0 || o.Jobs.MaxOpenJobs < 1 || o.Jobs.MaxOpenJobs > 1000 || o.Jobs.MaxHistory < 1 || o.Jobs.MaxHistory > 100000 || o.Jobs.AuditRetentionDays < 1 || o.Jobs.AuditRetentionDays > 3650 {
		return errors.New("invalid NAS history or resource bounds")
	}
	c.Hub.Storage = o.Storage
	c.Hub.Files = o.Files
	c.Hub.Jobs = o.Jobs
	c.Hub.Anime = o.Anime
	c.Guard.StartMemAvailableKB = o.GuardMemory.StartKB
	c.Guard.WarnMemAvailableKB = o.GuardMemory.WarnKB
	c.Guard.StopMemAvailableKB = o.GuardMemory.StopKB
	c.Guard.WorkerMemHighMB = o.GuardMemory.WorkerHighMB
	c.Guard.WorkerMemMaxMB = o.GuardMemory.WorkerMaxMB
	return c.Finalize()
}
func DecodeNASOptions(b []byte) (NASOverrides, error) {
	var o NASOverrides
	if len(b) > 65536 {
		return o, errors.New("NAS settings exceed 64 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e := d.Decode(&o); e != nil {
		return o, errors.New("invalid saved NAS settings")
	}
	if d.Decode(new(any)) != io.EOF {
		return o, errors.New("invalid saved NAS settings")
	}
	return o, nil
}
func ReadNASOptions(state string) ([]byte, error) {
	f, e := os.OpenFile(filepath.Join(state, NASSettingsFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Size() > 65536 {
		return nil, errors.New("NAS settings must be a private regular file within 64 KiB")
	}
	b, e := io.ReadAll(io.LimitReader(f, 65537))
	if len(b) > 65536 {
		return nil, errors.New("NAS settings exceed 64 KiB")
	}
	return b, e
}
func LoadNAS(path string) (*Config, error) {
	c, e := Load(path)
	if e != nil {
		return nil, e
	}
	b, e := ReadNASOptions(c.Paths.StateDir)
	if os.IsNotExist(e) {
		return c, nil
	}
	if e != nil {
		return nil, e
	}
	o, e := DecodeNASOptions(b)
	if e != nil {
		return nil, e
	}
	if e = ApplyNASOptions(c, o); e != nil {
		return nil, e
	}
	return c, nil
}
