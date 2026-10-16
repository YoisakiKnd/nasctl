// Package operations is the single registry of everything the console can be
// asked to do, together with the rules that govern each one: what it may touch,
// how risky it is, whether it needs approval, how long it may run, whether it is
// idempotent, whether it is asynchronous, and what it locks.
//
// There is exactly one registry, and the CLI, the HTTP API and (later) the MCP
// transport all consult it. This is deliberate: when each interface carries its
// own permission logic, the weakest interface becomes the real policy, and the
// difference is usually invisible until something goes wrong. Here a new
// interface cannot have a different policy, because it has no policy of its own.
package operations

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
)

// ParamKind is the declared type of one parameter.
type ParamKind string

const (
	ParamString ParamKind = "string"
	ParamInt    ParamKind = "int"
	ParamBool   ParamKind = "bool"
	ParamEnum   ParamKind = "enum"
)

// Param describes one accepted parameter. An undeclared parameter is a refusal,
// not something to ignore: silently dropping a parameter means the caller
// believes a bound was applied that never was.
type Param struct {
	Name     string    `json:"name"`
	Kind     ParamKind `json:"kind"`
	Required bool      `json:"required"`
	// MaxLen bounds a string parameter before it reaches any query or path
	// handling, so that a pathological value is rejected at the edge.
	MaxLen int `json:"max_len,omitempty"`
	// Enum lists the accepted values when Kind is ParamEnum.
	Enum []string `json:"enum,omitempty"`
	// Secret marks a parameter that must never be echoed into an audit row, a
	// page or an API response.
	Secret bool `json:"secret,omitempty"`
	// Doc is a short human explanation shown by `nasctl ops list`.
	Doc string `json:"doc,omitempty"`
}

// MutexClass names a mutual-exclusion group. Two operations that share a class
// cannot run at the same time; the job layer enforces it. Naming the class in
// the operation declaration is what stops two different code paths from
// inventing two different lock names for the same resource.
type MutexClass string

const (
	MutexNone   MutexClass = ""
	MutexScan   MutexClass = "scan"
	MutexAlbum  MutexClass = "album"
	MutexMedia  MutexClass = "media"
	MutexBackup MutexClass = "backup"
	MutexFiles  MutexClass = "files"
)

// Operation is one declarable action.
type Operation struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
	// Scope names what the operation can reach, in words an operator can audit:
	// "storage:<id>", "collection:<code>", "job:<id>", "suzu".
	Scope      []string            `json:"scope"`
	Class      domain.Risk         `json:"class"`
	Approval   domain.ApprovalRule `json:"approval"`
	Timeout    time.Duration       `json:"timeout"`
	Idempotent bool                `json:"idempotent"`
	// Async means the call returns a job ID immediately instead of doing the
	// work inline. Every heavy operation is async; a synchronous heavy
	// operation is a way to make the console hang.
	Async  bool       `json:"async"`
	Mutex  MutexClass `json:"mutex,omitempty"`
	Params []Param    `json:"params"`
	// ReadOnly marks operations that change nothing at all, here or elsewhere.
	// It is the property the default-deny policy for agents keys off.
	ReadOnly bool `json:"read_only"`
	// StopsWork permits a bounded stop/pause command during a paused mode. It
	// never bypasses identity, read-only, approval, mutex or job-cap checks.
	StopsWork bool `json:"stops_work,omitempty"`
	// MetadataOnly is limited to fixed, synchronous account/settings updates.
	// It permits maintenance while heavy work is paused; other gates remain.
	MetadataOnly bool `json:"metadata_only,omitempty"`
}

// Validate checks the declaration itself. A malformed operation is a programming
// error and is caught at startup, not at call time.
func (o Operation) Validate() error {
	if o.MetadataOnly {
		allowed := o.ID == "backup.webdav.configure" || o.ID == "apps.configure" || o.ID == "settings.configure" || o.ID == "users.configure" || o.ID == "users.password"
		if !allowed || o.Async || o.ReadOnly || o.Class != domain.RiskMutating || o.Timeout > 15*time.Second {
			return fmt.Errorf("operation %s: metadata_only requires an allowlisted synchronous bounded settings/account mutation", o.ID)
		}
	}
	if o.StopsWork && (o.ReadOnly || o.Async || o.Class != domain.RiskBounded || !strings.HasSuffix(o.ID, ".pause")) {
		return fmt.Errorf("operation %s: stops_work requires a synchronous bounded pause operation", o.ID)
	}
	if o.ID == "" {
		return fmt.Errorf("operation has no ID")
	}
	if o.Summary == "" {
		return fmt.Errorf("operation %s has no summary", o.ID)
	}
	if !o.Class.Valid() {
		return fmt.Errorf("operation %s has invalid risk class %q", o.ID, o.Class)
	}
	if !o.Approval.Valid() {
		return fmt.Errorf("operation %s has invalid approval rule %q", o.ID, o.Approval)
	}
	if o.Class == domain.RiskRead && o.Approval != domain.ApprovalNone {
		return fmt.Errorf("operation %s is read-only but requires %q approval; a read "+
			"operation must not need one, or agents cannot read", o.ID, o.Approval)
	}
	if o.Class != domain.RiskRead && o.Approval == domain.ApprovalNone {
		return fmt.Errorf("operation %s changes state but requires no approval", o.ID)
	}
	if o.ReadOnly && o.Class != domain.RiskRead {
		return fmt.Errorf("operation %s claims to be read-only but is classed %q", o.ID, o.Class)
	}
	if o.Class == domain.RiskRead && !o.ReadOnly {
		return fmt.Errorf("operation %s is classed read but does not set read_only", o.ID)
	}
	if o.Timeout <= 0 {
		return fmt.Errorf("operation %s has no timeout", o.ID)
	}
	if o.Async && o.Class == domain.RiskRead {
		return fmt.Errorf("operation %s is read-only but declared async; reads answer "+
			"from a bounded query, they do not create jobs", o.ID)
	}
	seen := map[string]bool{}
	for _, p := range o.Params {
		if p.Name == "" {
			return fmt.Errorf("operation %s has an unnamed parameter", o.ID)
		}
		if seen[p.Name] {
			return fmt.Errorf("operation %s declares parameter %q twice", o.ID, p.Name)
		}
		seen[p.Name] = true
		switch p.Kind {
		case ParamString:
			if p.MaxLen <= 0 {
				return fmt.Errorf("operation %s parameter %q is a string with no max_len", o.ID, p.Name)
			}
		case ParamInt, ParamBool:
		case ParamEnum:
			if len(p.Enum) == 0 {
				return fmt.Errorf("operation %s parameter %q is an enum with no values", o.ID, p.Name)
			}
		default:
			return fmt.Errorf("operation %s parameter %q has unknown kind %q", o.ID, p.Name, p.Kind)
		}
	}
	return nil
}

// Registry holds the declared operations.
type Registry struct {
	byID map[string]Operation
	ids  []string
}

// New builds a registry, validating every declaration and refusing duplicates.
func New(ops ...Operation) (*Registry, error) {
	r := &Registry{byID: make(map[string]Operation, len(ops))}
	for _, o := range ops {
		if err := o.Validate(); err != nil {
			return nil, err
		}
		if _, dup := r.byID[o.ID]; dup {
			return nil, fmt.Errorf("operation %s is declared twice", o.ID)
		}
		r.byID[o.ID] = o
		r.ids = append(r.ids, o.ID)
	}
	sort.Strings(r.ids)
	return r, nil
}

// Lookup returns the operation with the given ID.
func (r *Registry) Lookup(id string) (Operation, bool) {
	o, ok := r.byID[id]
	return o, ok
}

// All returns every operation, ordered by ID so that listings are stable.
func (r *Registry) All() []Operation {
	out := make([]Operation, 0, len(r.ids))
	for _, id := range r.ids {
		out = append(out, r.byID[id])
	}
	return out
}

// ReadOnly returns the IDs of every operation that changes nothing.
func (r *Registry) ReadOnly() []string {
	var out []string
	for _, id := range r.ids {
		if r.byID[id].ReadOnly {
			out = append(out, id)
		}
	}
	return out
}

// Args is a caller's parameters, already parsed into typed values.
type Args struct {
	strings map[string]string
	ints    map[string]int64
	bools   map[string]bool
}

// NewArgs returns an empty argument set.
func NewArgs() *Args {
	return &Args{
		strings: map[string]string{},
		ints:    map[string]int64{},
		bools:   map[string]bool{},
	}
}

// Str records a string parameter.
func (a *Args) Str(k, v string) *Args { a.strings[k] = v; return a }

// Int records an integer parameter.
func (a *Args) Int(k string, v int64) *Args { a.ints[k] = v; return a }

// Bool records a boolean parameter.
func (a *Args) Bool(k string, v bool) *Args { a.bools[k] = v; return a }

// String returns a string parameter.
func (a *Args) String(k string) string { return a.strings[k] }

// Int64 returns an integer parameter.
func (a *Args) Int64(k string) int64 { return a.ints[k] }

// BoolValue returns a boolean parameter.
func (a *Args) BoolValue(k string) bool { return a.bools[k] }

// Has reports whether a parameter was supplied at all.
func (a *Args) Has(k string) bool {
	if _, ok := a.strings[k]; ok {
		return true
	}
	if _, ok := a.ints[k]; ok {
		return true
	}
	if _, ok := a.bools[k]; ok {
		return true
	}
	return false
}

// Names returns the supplied parameter names, sorted.
func (a *Args) Names() []string {
	seen := map[string]bool{}
	var out []string
	for k := range a.strings {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range a.ints {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range a.bools {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Summary renders the arguments for an audit row: names and redacted values,
// never a secret. Sorting makes the summary stable, so two identical calls look
// identical in the log.
func (a *Args) Summary(op Operation) string {
	secret := map[string]bool{}
	for _, p := range op.Params {
		if p.Secret {
			secret[p.Name] = true
		}
	}
	var parts []string
	for _, k := range a.Names() {
		if secret[k] {
			parts = append(parts, k+"=[redacted]")
			continue
		}
		switch {
		case a.Has(k) && a.strings[k] != "":
			parts = append(parts, k+"="+domain.Redact(a.strings[k]))
		case a.ints[k] != 0:
			parts = append(parts, fmt.Sprintf("%s=%d", k, a.ints[k]))
		case a.bools[k]:
			parts = append(parts, k+"=true")
		default:
			if _, ok := a.strings[k]; ok {
				parts = append(parts, k+"=")
			}
		}
	}
	return strings.Join(parts, " ")
}

// Check validates a call against the declaration. It is the only place that
// decides whether a parameter set is acceptable.
func (o Operation) Check(a *Args) error {
	declared := map[string]Param{}
	for _, p := range o.Params {
		declared[p.Name] = p
	}
	for _, name := range a.Names() {
		p, ok := declared[name]
		if !ok {
			return fmt.Errorf("operation %s does not accept a parameter named %q", o.ID, name)
		}
		if p.Kind == ParamString {
			if v := a.String(name); len(v) > p.MaxLen {
				return fmt.Errorf("parameter %q is %d characters; the maximum for %s is %d",
					name, len(v), o.ID, p.MaxLen)
			}
		}
	}
	for _, p := range o.Params {
		if p.Required && !a.Has(p.Name) {
			return fmt.Errorf("operation %s requires the parameter %q", o.ID, p.Name)
		}
		if p.Kind == ParamEnum && a.Has(p.Name) {
			v := a.String(p.Name)
			if v == "" && !p.Required {
				continue
			}
			ok := false
			for _, allowed := range p.Enum {
				if v == allowed {
					ok = true
					break
				}
			}
			if !ok {
				return fmt.Errorf("parameter %q must be one of %s, got %q",
					p.Name, strings.Join(p.Enum, ", "), v)
			}
		}
	}
	return nil
}
