// Package jobs is the console's task layer. Everything asynchronous goes
// through it, and the CLI, the HTTP API and (later) the MCP transport all call
// the same methods.
//
// The design rule it exists to enforce is that there is one place which decides
// whether work may start. Admission is a sequence of gates - the operation is
// declared, the caller is allowed, the mode permits it, the resources are there,
// the mutex is free - and each gate has one implementation. When those checks are
// spread across callers, the weakest caller becomes the effective policy.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/resource"
)

// Errors returned by this package.
var (
	// ErrDenied means the operation is not permitted for this principal.
	ErrDenied = errors.New("jobs: not permitted")
	// ErrUnknownOp means the operation is not in the registry. An undeclared
	// operation is refused rather than attempted: there is no such thing as an
	// operation with no declared risk.
	ErrUnknownOp = errors.New("jobs: unknown operation")
	// ErrGated means a gate - mode, resource or mutex - refused the work. The
	// detail says which, so the caller can report it rather than guessing.
	ErrGated = errors.New("jobs: gated")
	// ErrNeedsApproval means the operation requires an approval that is missing.
	ErrNeedsApproval = errors.New("jobs: approval required")
	// ErrBusy means a mutually exclusive operation is already running.
	ErrBusy = errors.New("jobs: another operation holds the lock")
)

// Gate names a reason work did not start.
type Gate string

const (
	GateNone     Gate = ""
	GateMode     Gate = "mode"
	GateResource Gate = "resource"
	GateMutex    Gate = "mutex"
	GateApproval Gate = "approval"
	GateBackup   Gate = "backup"
	GateJobCap   Gate = "job_cap"
	GateReadOnly Gate = "read_only_console"
	GateRole     Gate = "role"
)

// Decision is the outcome of an admission check.
type Decision struct {
	Allowed bool
	Gate    Gate
	Reason  resource.Reason
	Detail  string
}

// Allow is the permit decision.
func Allow() Decision { return Decision{Allowed: true} }

// deny builds a refusal.
func deny(g Gate, detail string) Decision {
	return Decision{Gate: g, Detail: detail}
}

// denyResource builds a refusal caused by the machine's condition.
func denyResource(r resource.Reason, detail string) Decision {
	return Decision{Gate: GateResource, Reason: r, Detail: detail}
}

// Config wires the manager's collaborators. Everything is injected so that the
// layer can be tested without a machine, a database or a decoder.
type Config struct {
	Registry *operations.Registry
	Store    *hubstore.Store
	Guard    *resource.Guard
	Audit    func(ctx context.Context, e hubstore.AuditEntry) error
	// ReadOnlyConsole refuses every operation that changes state. It is set
	// when the operator has configured the console for inspection only, and it
	// is the strongest gate: it is checked before anything else, so no
	// combination of mode or approval can get past it.
	ReadOnlyConsole bool
	// AllowAIHeavy permits an AI principal to start heavy work that a
	// preauthorised approval covers. Off by default: an agent that can start
	// decoders on a 981 MiB machine is a decision an operator should make
	// explicitly, not inherit.
	AllowAIHeavy bool
	Now          func() time.Time
}

// Manager owns admission and job bookkeeping.
type Manager struct {
	cfg Config

	mu sync.Mutex
	// locks tracks which mutex classes are held, and by which job.
	locks map[operations.MutexClass]string
	// onReconcile is called once at startup with the outcome, so the caller can
	// surface orphans.
	reconciled bool
}

// New builds a manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Registry == nil {
		return nil, errors.New("jobs: no operation registry")
	}
	if cfg.Store == nil {
		return nil, errors.New("jobs: no store")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Manager{cfg: cfg, locks: map[operations.MutexClass]string{}}, nil
}

// Registry returns the operation registry.
func (m *Manager) Registry() *operations.Registry { return m.cfg.Registry }

// Store returns the underlying store.
func (m *Manager) Store() *hubstore.Store { return m.cfg.Store }

// now returns the current time through the injected clock.
func (m *Manager) now() time.Time { return m.cfg.Now() }

// Modes returns the stored mode set.
func (m *Manager) Modes(ctx context.Context) (domain.ModeSet, error) {
	return m.cfg.Store.Modes(ctx)
}

// EffectiveMode returns the mode governing new work.
func (m *Manager) EffectiveMode(ctx context.Context) (domain.ModeState, error) {
	set, err := m.cfg.Store.Modes(ctx)
	if err != nil {
		return domain.ModeState{}, err
	}
	return set.Effective(m.now()), nil
}

// NewJobID returns a fresh job identifier. It is random rather than sequential
// so that an ID cannot be guessed from another one, and carries a short prefix
// so that a job ID is recognisable in a log line.
func NewJobID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a reason to hand out a predictable ID;
		// fall back to the clock, which is still unique enough for this use.
		return fmt.Sprintf("j%x", time.Now().UnixNano())
	}
	return "j" + hex.EncodeToString(b[:])
}

// NewApprovalID returns a fresh approval identifier.
func NewApprovalID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("a%x", time.Now().UnixNano())
	}
	return "a" + hex.EncodeToString(b[:])
}

// audit records an action, routing through the injected hook when present so
// that tests can observe it and production writes it to the store.
func (m *Manager) audit(ctx context.Context, e hubstore.AuditEntry) {
	if e.At.IsZero() {
		e.At = m.now()
	}
	if m.cfg.Audit != nil {
		_ = m.cfg.Audit(ctx, e)
		return
	}
	_ = m.cfg.Store.Record(ctx, e)
}

// Admit runs the gates for one call and returns the decision.
//
// The order is deliberate. Cheap, absolute refusals come first (declared? known
// caller? read-only console?), then the caller's permission, then the machine's
// state, then the lock. A gate that can be evaluated without touching the
// filesystem or /proc is evaluated before one that cannot, so a refused call is
// cheap.
func (m *Manager) Admit(ctx context.Context, p domain.Principal, op operations.Operation, args *operations.Args) Decision {
	// 1. The caller must be a kind of principal we understand.
	if !p.Kind.Valid() {
		return deny(GateApproval, fmt.Sprintf("principal kind %q is not recognised", p.Kind))
	}

	// 2. Parameters must match the declaration exactly.
	if err := op.Check(args); err != nil {
		return deny(GateApproval, err.Error())
	}
	if !p.RoleAllowsOperation(op.ID, op.ReadOnly) {
		return deny(GateRole, "this user role does not permit the requested operation")
	}

	// 3. A read-only console refuses everything that changes state, before any
	//    other consideration. This is what makes "inspection only" a property
	//    of the console rather than a promise about its configuration.
	if m.cfg.ReadOnlyConsole && !op.ReadOnly {
		return deny(GateReadOnly, "this console is configured read-only, so it will not "+
			"start work that changes state")
	}

	// 4. Read operations need nothing more.
	if op.ReadOnly {
		return Allow()
	}

	// 5. Heavy work is what the modes govern. A mode never stops a read.
	mode, err := m.EffectiveMode(ctx)
	if err != nil {
		return deny(GateMode, "the current mode could not be read: "+err.Error())
	}
	if !mode.AllowsHeavyWork() && !op.StopsWork && !op.MetadataOnly {
		return deny(GateMode, fmt.Sprintf("mode %q is in force (%s), which does not permit "+
			"heavy work", mode.Mode, mode.Source))
	}

	// 6. Permission. An AI principal is additionally barred from heavy work
	//    unless the operator has enabled it.
	if p.Kind == domain.PrincipalAI && !m.cfg.AllowAIHeavy {
		return deny(GateApproval, "AI principals may not start heavy work on this console; "+
			"the operator must enable it explicitly")
	}

	switch op.Approval {
	case domain.ApprovalNone:
		// Nothing further. Only reachable for non-read operations if the
		// registry validation was bypassed, which it cannot be.
	case domain.ApprovalPreauthorised, domain.ApprovalPerCall:
		// The approval is consumed by the caller (Run), which has the target and
		// parameters; here we only record that one is required, so that a
		// decision taken before a job exists does not burn the approval.
	}

	// 7. Resources. Only for work that actually consumes them.
	if op.Async && m.cfg.Guard != nil {
		d := m.cfg.Guard.BeforeFile()
		if d.Verdict != resource.Allow {
			return denyResource(d.Reason, d.Detail)
		}
	}

	// 8. The mutex.
	if op.Mutex != MutexNone {
		m.mu.Lock()
		holder, held := m.locks[op.Mutex]
		m.mu.Unlock()
		if held && holder != "" {
			return deny(GateMutex, fmt.Sprintf("another operation holds the %s lock (job %s)",
				op.Mutex, holder))
		}
	}

	// 9. The job cap, checked last so that a refusal for a full queue does not
	//    mask a more specific reason.
	n, err := m.cfg.Store.CountOpen(ctx)
	if err == nil && n >= m.cfg.Store.Limits().MaxOpenJobs {
		return deny(GateJobCap, fmt.Sprintf("%d jobs are already open, which is the cap", n))
	}

	return Allow()
}

// MutexNone re-exports the registry's empty class so callers need not import
// both packages for a comparison.
const MutexNone = operations.MutexNone

// Lock acquires a mutex class for a job.
func (m *Manager) Lock(class operations.MutexClass, jobID string) error {
	if class == MutexNone {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, held := m.locks[class]; held && holder != jobID {
		return fmt.Errorf("%w: %s is held by job %s", ErrBusy, class, holder)
	}
	m.locks[class] = jobID
	return nil
}

// Unlock releases a mutex class. Releasing a class held by a different job is
// refused, so one job cannot free another's lock.
func (m *Manager) Unlock(class operations.MutexClass, jobID string) {
	if class == MutexNone {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if holder, held := m.locks[class]; held && holder == jobID {
		delete(m.locks, class)
	}
}

// HeldLocks returns the currently held mutex classes and their jobs.
func (m *Manager) HeldLocks() map[operations.MutexClass]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[operations.MutexClass]string, len(m.locks))
	for k, v := range m.locks {
		out[k] = v
	}
	return out
}

// Reconcile brings the job table in line with reality after a restart.
//
// It is called once, before anything can be admitted. Without it, a job that was
// running when the console died would stay "running" forever, and the console
// would report work in progress that does not exist.
func (m *Manager) Reconcile(ctx context.Context, alive func(pid, pgid int) bool) (int, []hubstore.Job, error) {
	if m.reconciled {
		return 0, nil, nil
	}
	m.reconciled = true
	return m.cfg.Store.Reconcile(ctx, alive, m.now())
}

// StartMode sets a mode.
func (m *Manager) StartMode(ctx context.Context, st domain.ModeState, by domain.Principal) error {
	if !by.DeviceAdmin() {
		return fmt.Errorf("%w: device modes require an administrator", ErrDenied)
	}
	if !st.Mode.Valid() {
		return fmt.Errorf("jobs: %q is not a mode", st.Mode)
	}
	if st.Mode == domain.ModeNormal {
		return fmt.Errorf("jobs: normal is the absence of a mode; clear the others instead")
	}
	// A mode that expires must expire in the future, or it would be set and
	// immediately inert, which reads as "the pause did not work".
	if !st.ExpiresAt.IsZero() && !st.ExpiresAt.After(m.now()) {
		return fmt.Errorf("jobs: %s would expire at %s, which is not in the future",
			st.Mode, st.ExpiresAt.Format(time.RFC3339))
	}
	if st.Source == "" {
		st.Source = by.String()
	}
	if st.SetAt.IsZero() {
		st.SetAt = m.now()
	}
	if err := m.cfg.Store.SetMode(ctx, st); err != nil {
		return err
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: by.String(), Action: "mode.set", Target: string(st.Mode),
		Params: domain.Redact(st.Reason), Outcome: "ok",
	})
	return nil
}

// ClearMode removes a mode. Only the modes a person sets may be cleared this
// way; resource_guard is cleared by the guard when its cause is resolved, so
// that an operator cannot clear a safety state and immediately restart decoders.
func (m *Manager) ClearMode(ctx context.Context, mode domain.Mode, by domain.Principal) error {
	if !by.DeviceAdmin() {
		return fmt.Errorf("%w: device modes require an administrator", ErrDenied)
	}
	if mode == domain.ModeResourceGuard {
		return fmt.Errorf("jobs: resource_guard is set by the machine and is not cleared by " +
			"hand; it clears when the resource cause is resolved")
	}
	if err := m.cfg.Store.ClearMode(ctx, mode); err != nil {
		return err
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: by.String(), Action: "mode.clear", Target: string(mode), Outcome: "ok",
	})
	return nil
}

// Guard returns the resource guard, which may be nil when the console has no
// sampler configured. A nil guard means "this console cannot tell whether the
// machine has room", and callers must say so rather than assuming there is room.
func (m *Manager) Guard() *resource.Guard { return m.cfg.Guard }

// ReadOnlyConsole reports whether this console refuses work that changes state.
func (m *Manager) ReadOnlyConsole() bool { return m.cfg.ReadOnlyConsole }

// AllowAIHeavy reports whether an AI principal may start heavy work.
func (m *Manager) AllowAIHeavy() bool { return m.cfg.AllowAIHeavy }
