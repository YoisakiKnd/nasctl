// Package webstate records the running web service in a small file so that
// `albumctl status` can answer two questions without talking to it: is the web
// service up, and what is it costing?
//
// The file is not a control channel and carries no authority. It is written by
// the service at startup and removed at shutdown; a file that outlives its
// process means the process was killed rather than stopped, which is worth
// showing an operator rather than hiding.
package webstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// FileName is the name of the record inside the run directory.
const FileName = "web.json"

// Record describes a running web service.
type Record struct {
	PID      int    `json:"pid"`
	Bind     string `json:"bind"`
	Started  string `json:"started"`
	Version  string `json:"version"`
	Schema   int    `json:"schema"`
	ReadOnly bool   `json:"read_only"`
}

// Path returns the record path for a run directory.
func Path(runDir string) string { return filepath.Join(runDir, FileName) }

// Write records the service, replacing any previous record atomically.
//
// The same temporary-file-then-rename discipline as everywhere else: a reader
// either sees the old record or the new one, never a half-written one.
func Write(runDir string, rec Record) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return fmt.Errorf("cannot create the run directory %s: %w", runDir, err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dst := Path(runDir)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("cannot replace %s: %w", dst, err)
	}
	return nil
}

// Read returns the recorded service, or an error if there is no usable record.
func Read(runDir string) (Record, error) {
	var rec Record
	b, err := os.ReadFile(Path(runDir))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, fmt.Errorf("%s is not a valid record: %w", Path(runDir), err)
	}
	if rec.PID <= 0 {
		return rec, fmt.Errorf("%s records no pid", Path(runDir))
	}
	return rec, nil
}

// Remove deletes the record. Called by the service on a clean shutdown.
func Remove(runDir string) error {
	err := os.Remove(Path(runDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ProcRoot returns the /proc mount to inspect, honouring the one environment
// override so that a test can point liveness checks at a fixture. Both the
// service and the CLI resolve it the same way.
func ProcRoot() string {
	if p := os.Getenv("ALBUMCTL_PROC_ROOT"); p != "" {
		return p
	}
	return "/proc"
}

// Alive reports whether pid is running as an albumctl process.
//
// The name check matters: pids are reused, and reporting a stranger's memory as
// the web service's would be worse than reporting nothing.
func Alive(procRoot string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	return trimName(string(b)) == "albumctl"
}

// StartedAt parses the recorded start time, or returns the zero time.
func (r Record) StartedAt() time.Time {
	t, err := time.Parse(time.RFC3339, r.Started)
	if err != nil {
		return time.Time{}
	}
	return t
}

func trimName(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == ' ' {
			return s[:i]
		}
	}
	return s
}
