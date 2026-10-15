// Package backups defines startup-injected local backup capabilities. HTTP
// handlers never receive filesystem handles or a path outside configured roots.
package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var ErrBusy = errors.New("backup: another backup operation is active")
var ErrInvalid = errors.New("backup: invalid or changed plan, snapshot or restore scope")
var ErrCorrupt = errors.New("backup: content or manifest validation failed")
var ErrFull = errors.New("backup: version, tree or manifest budget reached")

type Plan struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Root        string `json:"root"`
	Path        string `json:"path"`
	ToRoot      string `json:"to_root"`
	Limit       int    `json:"limit"`
	MaxBytes    int64  `json:"max_bytes"`
	Keep        int    `json:"keep"`
	MaxRuntime  string `json:"max_runtime"`
	SourceScope string `json:"source_scope"`
	TargetScope string `json:"target_scope"`
}

func (p Plan) Digest() string {
	b, _ := json.Marshal(p)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type Settings struct {
	Plans    []Plan            `json:"plans"`
	Digests  map[string]string `json:"digests"`
	Busy     bool              `json:"busy"`
	MaxPlans int               `json:"max_plans"`
}
type Snapshot struct {
	ID             string `json:"id"`
	PlanID         string `json:"plan_id"`
	Name           string `json:"name"`
	Root           string `json:"root"`
	Path           string `json:"path"`
	ToRoot         string `json:"to_root"`
	CreatedAt      int64  `json:"created_at"`
	Entries        int64  `json:"entries"`
	Bytes          int64  `json:"bytes"`
	Skipped        int    `json:"skipped_internal"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Deleting       bool   `json:"deleting"`
}
type History struct {
	Snapshots  []Snapshot `json:"snapshots"`
	Incomplete []string   `json:"incomplete"`
	Truncated  bool       `json:"truncated"`
}
type Progress struct {
	Phase          string `json:"phase"`
	Entries, Bytes int64
	Detail         string
}
type Result struct {
	Snapshot        *Snapshot `json:"snapshot,omitempty"`
	Published       bool      `json:"published"`
	DestinationRoot string    `json:"destination_root,omitempty"`
	Path            string    `json:"path,omitempty"`
	Pruned          int       `json:"pruned"`
	Warning         string    `json:"warning,omitempty"`
}
type Restore struct {
	Root           string `json:"root"`
	ID             string `json:"id"`
	ToRoot         string `json:"to_root"`
	To             string `json:"to"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type Control interface {
	Settings() Settings
	Save(context.Context, Plan) error
	RemovePlan(context.Context, string) error
	History(context.Context, string) (History, error)
	Run(context.Context, string, string, func(Progress)) (Result, error)
	Verify(context.Context, string, string, func(Progress)) (Result, error)
	Restore(context.Context, Restore, func(Progress)) (Result, error)
	Delete(context.Context, string, string, string, func(Progress)) (Result, error)
	Cleanup(context.Context, string, string, func(Progress)) (Result, error)
}
