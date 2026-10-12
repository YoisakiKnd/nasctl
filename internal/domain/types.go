// Package domain holds the vocabulary the console is built from: modes, job
// states, risk classes and principals. It deliberately has no dependencies on
// anything else in this module and performs no I/O, so that the rules about what
// a state means can be tested without a database, a filesystem or a process.
//
// The reason this is its own package rather than a few types scattered around is
// the failure mode it prevents: when "paused", "waiting" and "unknown" are each
// defined where they are first used, the web layer ends up deciding what they
// mean, and the answer differs between the page, the API and the CLI. Here there
// is one definition, and every consumer asks it.
package domain

import (
	"fmt"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Modes
// ---------------------------------------------------------------------------

// Mode is the machine's operating intent. It answers one question: what work is
// the console willing to start right now?
//
// A mode is not a command. It never signals a process, never edits a timer and
// never stops another service - it only gates what this application is prepared
// to begin. That is what makes "watching mode" safe to offer at all: if the
// console is wrong about the mode, the worst outcome is that our own work waits.
type Mode string

const (
	// ModeNormal is the absence of any other mode.
	ModeNormal Mode = "normal"

	// ModeWatching is a temporary "someone is watching something" window. It
	// pauses album heavy work and may slow a transfer, and it expires on its
	// own so that a forgotten window cannot silently stop maintenance forever.
	ModeWatching Mode = "watching"

	// ModeMaintenance is an operator-declared window for bounded maintenance.
	// It permits work that is otherwise gated by scheduling policy, but never
	// bypasses a resource check.
	ModeMaintenance Mode = "maintenance"

	// ModeManualPause is a human decision to stop this application's heavy work.
	// It is durable, it outranks every automatic mode, and only a human clears
	// it. Automation must never be able to lift it.
	ModeManualPause Mode = "manual_pause"

	// ModeResourceGuard is set by the machine, not by a person: a resource trip
	// happened and nothing heavy may start until the cause is understood. It
	// outranks everything except the safety checks themselves.
	ModeResourceGuard Mode = "resource_guard"
)

// Valid reports whether m is a mode this program understands.
func (m Mode) Valid() bool {
	switch m {
	case ModeNormal, ModeWatching, ModeMaintenance, ModeManualPause, ModeResourceGuard:
		return true
	}
	return false
}

// ModeState is a mode together with where it came from and when it ends.
type ModeState struct {
	Mode   Mode      `json:"mode"`
	Source string    `json:"source"`           // "user:alice", "system:resource_guard"
	Reason string    `json:"reason,omitempty"` // why, in the words of whoever set it
	SetAt  time.Time `json:"set_at"`
	// ExpiresAt is zero for modes that do not expire on their own. A mode with
	// an expiry is a promise that the machine returns to normal without anyone
	// remembering to do it.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Expired reports whether an expiring mode has run out at the given instant.
func (s ModeState) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// Normal returns a state representing the absence of any mode.
//
// It exists so that a caller reporting "no mode is in force" reports a
// well-formed value rather than the zero ModeState, whose SetAt is year 1 and
// whose Source is empty - details that show up in a page as a nonsense timestamp
// and an unlabelled source.
func Normal() ModeState {
	return ModeState{Mode: ModeNormal, Source: "default"}
}

// Active reports whether this mode is still in force at the given instant.
func (s ModeState) Active(now time.Time) bool {
	if s.Mode == "" || s.Mode == ModeNormal {
		return false
	}
	return !s.Expired(now)
}

// ModeSet is the current set of mode states, most significant first.
type ModeSet struct {
	States []ModeState `json:"states"`
}

// Effective returns the mode that governs new work at the given instant, and the
// state that carries it.
//
// Precedence, highest first:
//
//	resource_guard  - the machine said stop; a human window cannot overrule it
//	manual_pause    - a human said stop; an automatic window cannot overrule it
//	watching        - a temporary window that only ever *restricts*
//	maintenance     - a temporary window that only ever *permits*
//	normal
//
// The two restricting modes outrank the permissive one on purpose. A maintenance
// window that overlapped a watching window must not license heavy work while
// somebody is streaming, so restriction wins every tie.
func (m ModeSet) Effective(now time.Time) ModeState {
	best := Normal()
	bestRank := 0
	for _, s := range m.States {
		if !s.Active(now) {
			continue
		}
		if r := s.Mode.rank(); r > bestRank {
			best, bestRank = s, r
		}
	}
	return best
}

// rank orders modes by how much they restrict work. Higher wins.
func (m Mode) rank() int {
	switch m {
	case ModeResourceGuard:
		return 5
	case ModeManualPause:
		return 4
	case ModeWatching:
		return 3
	case ModeMaintenance:
		return 2
	case ModeNormal:
		return 1
	}
	return 0
}

// AllowsHeavyWork reports whether the effective mode permits heavy work to be
// admitted. It is the only place that decision is made.
func (s ModeState) AllowsHeavyWork() bool {
	switch s.Mode {
	case ModeNormal, ModeMaintenance:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Job states
// ---------------------------------------------------------------------------

// JobState is the lifecycle position of one asynchronous request.
//
// The vocabulary is closed and every state is distinguishable in the UI, because
// the failure this guards against is a console that shows one comforting word
// ("running", or worse, a green tick) for a job whose real state is unknown.
type JobState string

const (
	// JobQueued means accepted and recorded, not started. "Accepted" is not
	// "started", and the UI must not blur the two.
	JobQueued JobState = "queued"
	// JobRunning means an instance exists right now.
	JobRunning JobState = "running"
	// JobWaiting means it is gated - by a mode, a resource check, a lock or a
	// busy backup - and will proceed on its own if the gate opens.
	JobWaiting JobState = "waiting"
	// JobPaused means a human stopped it; unlike waiting, nothing resumes it by
	// itself.
	JobPaused JobState = "paused"
	// JobCompleted means it finished and was verified.
	JobCompleted JobState = "completed"
	// JobFailed means it finished with an error.
	JobFailed JobState = "failed"
	// JobCancelled means a caller stopped it before completion.
	JobCancelled JobState = "cancelled"
	// JobInterrupted means the console died while it was running. The next
	// startup reconciles these; until then the honest answer is "interrupted",
	// not "running" and not "failed".
	JobInterrupted JobState = "interrupted"
	// JobUnknown means reconciliation could not decide. It must never be
	// rendered as healthy, and the UI gives it its own colour.
	JobUnknown JobState = "unknown"
)

// Valid reports whether s is a state this program understands.
func (s JobState) Valid() bool {
	switch s {
	case JobQueued, JobRunning, JobWaiting, JobPaused, JobCompleted,
		JobFailed, JobCancelled, JobInterrupted, JobUnknown:
		return true
	}
	return false
}

// Terminal reports whether the state is final. A terminal job is never resumed;
// a new request creates a new job, so that history stays append-only.
func (s JobState) Terminal() bool {
	switch s {
	case JobCompleted, JobFailed, JobCancelled:
		return true
	}
	return false
}

// Open reports whether the job still occupies a slot.
func (s JobState) Open() bool {
	return !s.Terminal()
}

// NeedsAttention reports whether the state should be surfaced as a problem
// rather than as normal operation. Unknown and interrupted qualify: they are
// precisely the states where the console does not know what happened.
func (s JobState) NeedsAttention() bool {
	switch s {
	case JobFailed, JobInterrupted, JobUnknown:
		return true
	}
	return false
}

// canTransition is the state machine's edge set. It is a table rather than a
// chain of ifs so that an illegal transition is a lookup miss, which is easy to
// test exhaustively.
var canTransition = map[JobState][]JobState{
	JobQueued:      {JobRunning, JobWaiting, JobPaused, JobCancelled, JobFailed, JobUnknown},
	JobRunning:     {JobCompleted, JobFailed, JobCancelled, JobPaused, JobWaiting, JobInterrupted, JobUnknown},
	JobWaiting:     {JobRunning, JobQueued, JobPaused, JobCancelled, JobFailed, JobUnknown},
	JobPaused:      {JobRunning, JobQueued, JobCancelled, JobFailed, JobUnknown},
	JobInterrupted: {JobUnknown, JobFailed, JobCancelled},
	JobUnknown:     {JobFailed, JobCancelled, JobCompleted, JobInterrupted},
	// terminal states have no outgoing edges
	JobCompleted: {},
	JobFailed:    {},
	JobCancelled: {},
}

// Transition reports whether moving from s to next is allowed.
func (s JobState) Transition(next JobState) error {
	if !next.Valid() {
		return fmt.Errorf("job state %q is not a known state", next)
	}
	for _, allowed := range canTransition[s] {
		if allowed == next {
			return nil
		}
	}
	if s == next {
		// Idempotent writes are normal: a reconcile pass may re-assert the
		// state it already recorded.
		return nil
	}
	return fmt.Errorf("job cannot move from %q to %q", s, next)
}

// ---------------------------------------------------------------------------
// Principals, risk and approval
// ---------------------------------------------------------------------------

// PrincipalKind distinguishes who is asking. The separation is not cosmetic: an
// AI agent's credential must not be able to do what a human's can, and neither
// may approve its own request.
type PrincipalKind string

const (
	PrincipalUser   PrincipalKind = "user"   // a human, through the console or the API
	PrincipalAI     PrincipalKind = "ai"     // an agent, with its own credential
	PrincipalLocal  PrincipalKind = "local"  // the operator at the machine, via the CLI
	PrincipalHelper PrincipalKind = "helper" // a privileged helper, fixed operations only
)

// Valid reports whether k is a known principal kind.
func (k PrincipalKind) Valid() bool {
	switch k {
	case PrincipalUser, PrincipalAI, PrincipalLocal, PrincipalHelper:
		return true
	}
	return false
}

// Principal is an authenticated caller.
type Principal struct {
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
}

// String renders the principal in the form used in job and audit rows, so that
// the same caller looks the same everywhere.
func (p Principal) String() string {
	if p.Name == "" {
		return string(p.Kind)
	}
	return string(p.Kind) + ":" + p.Name
}

// Risk classifies what an operation can do to the machine or its data. It is
// declared by the operation, never inferred from its name.
type Risk string

const (
	// RiskRead only reads. It cannot change anything, here or elsewhere.
	RiskRead Risk = "read"
	// RiskBounded does real work with a declared, enforced bound on count and
	// runtime, and no effect on user data (a bounded scan, a bounded batch of
	// thumbnails).
	RiskBounded Risk = "bounded"
	// RiskMutating changes files or state that the user cares about.
	RiskMutating Risk = "mutating"
	// RiskPrivileged changes the machine or the console's own scheduling:
	// service restarts, enabling schedules, widening a library.
	RiskPrivileged Risk = "privileged"
)

// Valid reports whether r is a known risk class.
func (r Risk) Valid() bool {
	switch r {
	case RiskRead, RiskBounded, RiskMutating, RiskPrivileged:
		return true
	}
	return false
}

// ApprovalRule says what a call needs before it may run.
type ApprovalRule string

const (
	// ApprovalNone is for read operations.
	ApprovalNone ApprovalRule = "none"
	// ApprovalPreauthorised means an operator may grant a standing, scoped and
	// expiring permission for this operation to a principal (for example: an
	// agent may start a bounded scan of one collection for the next hour).
	ApprovalPreauthorised ApprovalRule = "preauthorised"
	// ApprovalPerCall means every single call needs its own approval bound to
	// the exact target and parameters.
	ApprovalPerCall ApprovalRule = "per_call"
)

// Valid reports whether a is a known rule.
func (a ApprovalRule) Valid() bool {
	switch a {
	case ApprovalNone, ApprovalPreauthorised, ApprovalPerCall:
		return true
	}
	return false
}

// Redact prepares a string for an audit row: it keeps the shape of a value but
// not a secret. Callers pass parameter values through this before recording
// them, and anything that looks like a credential is replaced wholesale.
func Redact(s string) string {
	if looksSecret(s) {
		return "[redacted]"
	}
	const max = 200
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// secretMarkers are substrings that mark a value as credential-shaped. They are
// matched case-insensitively; the list is intentionally short and blunt, because
// the cost of over-redacting an audit field is nil and the cost of leaking a
// token into a log is not.
var secretMarkers = []string{
	"token", "secret", "password", "passwd", "bearer",
	// "apikey" as one word, and the three separators a human actually types.
	// Missing one of these means a credential reaches an audit row in a
	// spelling nobody thought to check.
	"apikey", "api_key", "api-key", "api.key",
}

func looksSecret(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range secretMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
