package statebackup

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

// Offline operations cannot open the hub writer while holding its exclusive
// lease. Keep a separate bounded private audit, and do not restore this audit
// from an older package. The caller must hold all offline leases.
type auditRow struct {
	At             int64  `json:"at"`
	Action         string `json:"action"`
	PackageID      string `json:"package_id,omitempty"`
	ManifestSHA256 string `json:"manifest_sha256,omitempty"`
	CheckpointID   string `json:"checkpoint_id,omitempty"`
	Outcome        string `json:"outcome"`
}

func audit(l Layout, action string, result Result, workErr error) error {
	p := filepath.Join(l.State, "offline-state-audit.json")
	rows := []auditRow{}
	b, e := readPrivate(p, 65536)
	if e == nil {
		if e = decode(b, &rows); e != nil {
			return e
		}
		if len(rows) > 100 {
			return ErrInvalid
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	outcome := "ok"
	if workErr != nil {
		outcome = "failed"
	}
	if len(action) > 6 && action[len(action)-6:] == ".start" {
		outcome = "started"
	}
	r := auditRow{At: time.Now().Unix(), Action: action, PackageID: result.ID, ManifestSHA256: result.ManifestSHA256, Outcome: outcome}
	if result.Checkpoint != "" {
		r.CheckpointID = filepath.Base(result.Checkpoint)
	}
	rows = append(rows, r)
	if len(rows) > 100 {
		rows = rows[len(rows)-100:]
	}
	b, e = json.Marshal(rows)
	if e != nil || len(b) > 65536 {
		return ErrInvalid
	}
	return fsutil.AtomicWrite(p, 0600, func(w io.Writer) error { _, e := w.Write(b); return e })
}
