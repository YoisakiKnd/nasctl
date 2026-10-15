package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubops"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/resource"
)

// rig builds a manager over a temporary store, with a clock the test controls.
type rig struct {
	m      *Manager
	store  *hubstore.Store
	now    time.Time
	audits []hubstore.AuditEntry
}

func newRig(t *testing.T, tune func(*Config, *hubstore.Limits)) *rig {
	t.Helper()
	limits := hubstore.DefaultLimits()
	cfg := Config{}
	if tune != nil {
		tune(&cfg, &limits)
	}
	st, err := hubstore.Open(filepath.Join(t.TempDir(), "hub.db"), limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	reg, err := operations.New(hubops.Declarations()...)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{store: st, now: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg.Registry = reg
	cfg.Store = st
	cfg.Now = func() time.Time { return r.now }
	cfg.Audit = func(_ context.Context, e hubstore.AuditEntry) error {
		r.audits = append(r.audits, e)
		return nil
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	return r
}

func TestCancelledWorkRecordsItsTerminalState(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	out := r.m.Run(ctx, RunOptions{Principal: domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}, OperatorConsent: true, OpID: "files.mkdir", Target: "data", Args: operations.NewArgs().Str("root", "data").Str("path", "new"), Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (Result, error) {
		cancel()
		return Result{}, ctx.Err()
	}})
	if !errors.Is(out.Err, context.Canceled) || out.Job == nil || out.Job.State != domain.JobCancelled {
		t.Fatalf("cancelled result: %+v", out)
	}
	stored, err := r.store.Job(context.Background(), out.Job.ID)
	if err != nil || stored.State != domain.JobCancelled || stored.StopReason != "cancelled" {
		t.Fatalf("durable cancellation: %+v %v", stored, err)
	}
}

func TestTerminalJobReleasesMutexBeforeFinalAuditCompletes(t *testing.T) {
	r := newRig(t, nil)
	auditing, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	r.m.cfg.Audit = func(_ context.Context, e hubstore.AuditEntry) error {
		if e.Action == "call" && e.Outcome == "ok" && e.Target == "first" {
			close(auditing)
			<-release
		}
		return nil
	}
	args := operations.NewArgs().Str("root", "data").Str("to_root", "other").Str("from", "one").Str("to", "two").Int("limit", 10).Int("max_bytes", 1024)
	accepted := make(chan string, 1)
	finished := make(chan Outcome, 1)
	principal := domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}
	go func() {
		finished <- r.m.Run(context.Background(), RunOptions{Principal: principal, OpID: "files.copy", Args: args, Target: "first", OperatorConsent: true, OnAccepted: func(j *hubstore.Job) { accepted <- j.ID }, Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true, Verified: true}, nil
		}})
	}()
	select {
	case <-auditing:
	case <-time.After(3 * time.Second):
		t.Fatal("final audit never reached")
	}
	var id string
	select {
	case id = <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("audit blocked before job acceptance")
	}
	job, err := r.store.Job(context.Background(), id)
	if err != nil || !job.State.Terminal() {
		t.Fatal(job, err)
	}
	op, _ := r.m.Registry().Lookup("files.copy")
	if decision := r.m.Admit(context.Background(), principal, op, args); !decision.Allowed {
		t.Fatal("terminal job still blocks next operation", decision)
	}
	close(release)
	select {
	case out := <-finished:
		if out.Err != nil {
			t.Fatal(out.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not finish")
	}
}

func TestOperatorConsentNeverApprovesAIHelpersOrPrivilegedCalls(t *testing.T) {
	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.AllowAIHeavy = true })
	for _, kind := range []domain.PrincipalKind{domain.PrincipalAI, domain.PrincipalHelper} {
		called := false
		out := r.m.Run(context.Background(), RunOptions{Principal: domain.Principal{Kind: kind, Name: "test"}, OperatorConsent: true, OpID: "files.mkdir", Target: "data", Args: operations.NewArgs().Str("root", "data").Str("path", "new"), Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			called = true
			return Result{Completed: true}, nil
		}})
		if called || !errors.Is(out.Err, ErrNeedsApproval) {
			t.Fatalf("consent promoted %s: %+v", kind, out)
		}
	}
	out := r.m.Run(context.Background(), RunOptions{Principal: domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}, OperatorConsent: true, OpID: "schedule.disable", Target: "system", Args: operations.NewArgs(), Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
		t.Fatal("consent approved privileged work")
		return Result{}, nil
	}})
	if !errors.Is(out.Err, ErrNeedsApproval) {
		t.Fatal("privileged operation accepted", out.Err)
	}
}

func (r *rig) ctx() context.Context { return context.Background() }

// findAudit returns the last audit row for an operation.
func (r *rig) lastAudit() hubstore.AuditEntry {
	if len(r.audits) == 0 {
		return hubstore.AuditEntry{}
	}
	return r.audits[len(r.audits)-1]
}

func op(t *testing.T, id string) operations.Operation {
	t.Helper()
	reg, err := operations.New(hubops.Declarations()...)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := reg.Lookup(id)
	if !ok {
		t.Fatalf("operation %s is not declared", id)
	}
	return o
}

// TestReadsAreAlwaysAllowed: the default posture for an agent is read-only, and
// that is only meaningful if reads actually work with no approval, no mode and no
// operator involvement.
func TestReadsAreAlwaysAllowed(t *testing.T) {
	r := newRig(t, nil)
	args := operations.NewArgs().Str("root", "data")
	d := r.m.Admit(r.ctx(), domain.Principal{Kind: domain.PrincipalAI, Name: "hermes"},
		op(t, "files.list"), args)
	if !d.Allowed {
		t.Fatalf("a read was refused: gate=%s %s", d.Gate, d.Detail)
	}
}

// TestAnUndeclaredParameterIsRefused: silently dropping an unknown parameter
// means the caller believes a bound was applied that never was.
func TestAnUndeclaredParameterIsRefused(t *testing.T) {
	r := newRig(t, nil)
	args := operations.NewArgs().Str("root", "data").Str("sudo", "true")
	d := r.m.Admit(r.ctx(), domain.Principal{Kind: domain.PrincipalLocal},
		op(t, "files.list"), args)
	if d.Allowed {
		t.Fatal("an undeclared parameter was accepted")
	}
}

// TestAManualPauseStopsHeavyWorkButNotReads is the central mode property: a pause
// stops decoders, and does not take the console away from the operator.
func TestAManualPauseStopsHeavyWorkButNotReads(t *testing.T) {
	r := newRig(t, nil)
	if err := r.store.SetMode(r.ctx(), domain.ModeState{
		Mode: domain.ModeManualPause, Source: "user:op", SetAt: r.now,
	}); err != nil {
		t.Fatal(err)
	}
	p := domain.Principal{Kind: domain.PrincipalUser, Name: "op"}

	d := r.m.Admit(r.ctx(), p, op(t, "album.scan"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 10))
	if d.Allowed {
		t.Fatal("heavy work was admitted while a manual pause was in force")
	}
	if d.Gate != GateMode {
		t.Errorf("gate = %q, want %q", d.Gate, GateMode)
	}

	d = r.m.Admit(r.ctx(), p, op(t, "album.timeline"), operations.NewArgs())
	if !d.Allowed {
		t.Errorf("a read was refused under a manual pause: %s", d.Detail)
	}
}

// TestAResourceTripOutranksAMaintenanceWindow: a maintenance window permits work
// in principle; it does not override the machine's own condition.
func TestAResourceTripOutranksAMaintenanceWindow(t *testing.T) {
	r := newRig(t, nil)
	if err := r.store.SetMode(r.ctx(), domain.ModeState{
		Mode: domain.ModeMaintenance, Source: "user:op", SetAt: r.now,
		ExpiresAt: r.now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.SetMode(r.ctx(), domain.ModeState{
		Mode: domain.ModeResourceGuard, Source: "system", SetAt: r.now,
	}); err != nil {
		t.Fatal(err)
	}
	d := r.m.Admit(r.ctx(), domain.Principal{Kind: domain.PrincipalLocal}, op(t, "album.scan"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 10))
	if d.Allowed {
		t.Fatal("a maintenance window overrode a resource trip")
	}
	if d.Gate != GateMode {
		t.Errorf("gate = %q, want %q", d.Gate, GateMode)
	}
}

// TestAReadOnlyConsoleRefusesEveryWrite: the read-only flag must be the
// strongest gate, so that "inspection only" cannot be defeated by a mode or an
// approval.
func TestAReadOnlyConsoleRefusesEveryWrite(t *testing.T) {
	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.ReadOnlyConsole = true })
	p := domain.Principal{Kind: domain.PrincipalLocal}

	// Even with an approval in place, a write is refused.
	if err := r.store.GrantApproval(r.ctx(), hubstore.Approval{
		ID: "ap", Op: "files.mkdir", Principal: p.String(), Target: "data/x", Params: "",
		GrantedBy: "user:op", GrantedAt: r.now, ExpiresAt: r.now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	d := r.m.Admit(r.ctx(), p, op(t, "files.mkdir"),
		operations.NewArgs().Str("root", "data").Str("path", "x"))
	if d.Allowed {
		t.Fatal("a read-only console admitted a write")
	}
	if d.Gate != GateReadOnly {
		t.Errorf("gate = %q, want %q: the read-only gate must be checked before approval",
			d.Gate, GateReadOnly)
	}

	// Reads still work, which is what makes read-only mode useful.
	if d := r.m.Admit(r.ctx(), p, op(t, "files.list"),
		operations.NewArgs().Str("root", "data")); !d.Allowed {
		t.Errorf("a read-only console refused a read: %s", d.Detail)
	}
}

// TestAnAICannotStartHeavyWorkByDefault: on a 981 MiB machine, an agent that can
// start decoders is a decision the operator must make deliberately.
func TestAnAICannotStartHeavyWorkByDefault(t *testing.T) {
	r := newRig(t, nil)
	ai := domain.Principal{Kind: domain.PrincipalAI, Name: "hermes"}
	d := r.m.Admit(r.ctx(), ai, op(t, "album.scan"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 10))
	if d.Allowed {
		t.Fatal("an AI principal was allowed to start heavy work by default")
	}
	if d.Gate != GateApproval {
		t.Errorf("gate = %q, want %q", d.Gate, GateApproval)
	}

	// With the operator's explicit switch on, the same call gets further: it now
	// reaches the approval requirement instead.
	r2 := newRig(t, func(c *Config, _ *hubstore.Limits) { c.AllowAIHeavy = true })
	d = r2.m.Admit(r2.ctx(), ai, op(t, "album.scan"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 10))
	if !d.Allowed {
		t.Fatalf("with allow_ai_heavy the call was still refused at admission: %s", d.Detail)
	}
}

func TestPauseCommandsRemainPossibleDuringPausedModesWithoutBypassingOtherGates(t *testing.T) {
	for _, readonly := range []bool{false, true} {
		r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.ReadOnlyConsole = readonly })
		operator := domain.Principal{Kind: domain.PrincipalUser, Name: "operator"}
		if err := r.store.SetMode(r.ctx(), domain.ModeState{Mode: domain.ModeManualPause, Source: "test", SetAt: r.now}); err != nil {
			t.Fatal(err)
		}
		args := operations.NewArgs().Int("episode", 1)
		d := r.m.Admit(r.ctx(), operator, op(t, "anime.download.pause"), args)
		if readonly {
			if d.Gate != GateReadOnly {
				t.Fatal(d)
			}
		} else if !d.Allowed {
			t.Fatal(d)
		}
		resume := r.m.Admit(r.ctx(), operator, op(t, "anime.download.resume"), args)
		if resume.Allowed {
			t.Fatal("resume bypassed paused mode")
		}
		ai := r.m.Admit(r.ctx(), domain.Principal{Kind: domain.PrincipalAI, Name: "agent"}, op(t, "anime.download.pause"), args)
		if ai.Allowed {
			t.Fatal("pause bypassed AI restriction")
		}
	}
}

// TestAnAICannotGrantItsOwnApproval is the rule that makes approval meaningful.
// If an agent could approve its own request, the approval step would be a
// formality and the operator's consent would be fictional.
func TestAnAICannotGrantItsOwnApproval(t *testing.T) {
	r := newRig(t, nil)
	ai := domain.Principal{Kind: domain.PrincipalAI, Name: "hermes"}

	_, err := r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "photos", Params: "limit=10",
		GrantedBy: ai, ExpiresAt: r.now.Add(time.Hour), OneShot: true,
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("an AI granted itself an approval: error = %v, want ErrDenied", err)
	}

	// An agent cannot approve another agent either: the granter must be a person.
	other := domain.Principal{Kind: domain.PrincipalAI, Name: "other"}
	_, err = r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: other, Target: "photos", Params: "limit=10",
		GrantedBy: ai, ExpiresAt: r.now.Add(time.Hour), OneShot: true,
	})
	if !errors.Is(err, ErrDenied) {
		t.Errorf("an AI granted an approval to another AI: %v", err)
	}
}

// TestAHelperGrantsNothing: a privileged helper executes fixed operations; it is
// not an authority that can widen its own mandate.
func TestAHelperGrantsNothing(t *testing.T) {
	r := newRig(t, nil)
	_, err := r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: domain.Principal{Kind: domain.PrincipalAI, Name: "h"},
		Target: "photos", Params: "limit=10",
		GrantedBy: domain.Principal{Kind: domain.PrincipalHelper, Name: "helper"},
		ExpiresAt: r.now.Add(time.Hour), OneShot: true,
	})
	if !errors.Is(err, ErrDenied) {
		t.Errorf("a helper granted an approval: %v", err)
	}
}

// TestAnApprovalMustBeScopedAndExpire: an approval with no expiry or no target is
// a permanent, unbounded permission - exactly what the model exists to avoid.
func TestAnApprovalMustBeScopedAndExpire(t *testing.T) {
	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.AllowAIHeavy = true })
	opr := domain.Principal{Kind: domain.PrincipalUser, Name: "op"}
	ai := domain.Principal{Kind: domain.PrincipalAI, Name: "hermes"}

	_, err := r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "photos", Params: "limit=10",
		GrantedBy: opr, // no expiry
	})
	if err == nil {
		t.Error("an approval with no expiry was granted")
	}

	_, err = r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "", Params: "limit=10",
		GrantedBy: opr, ExpiresAt: r.now.Add(time.Hour),
	})
	if err == nil {
		t.Error("an approval naming no target was granted for an operation that acts on one")
	}

	// Longer than the maximum is refused too.
	_, err = r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "photos", Params: "limit=10",
		GrantedBy: opr, ExpiresAt: r.now.Add(72 * time.Hour),
	})
	if err == nil {
		t.Error("an approval lasting three days was granted")
	}

	// A well-formed one is accepted.
	a, err := r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "photos", Params: "limit=10",
		GrantedBy: opr, ExpiresAt: r.now.Add(time.Hour), OneShot: true,
	})
	if err != nil {
		t.Fatalf("a valid approval was refused: %v", err)
	}
	if a.GrantedBy != "user:op" {
		t.Errorf("granted_by = %q, want user:op", a.GrantedBy)
	}
}

// TestAnApprovalDoesNotCoverAChangedParameter: the bound the operator agreed to
// must survive. Approving "scan 10 files" is not approving "scan".
func TestAnApprovalDoesNotCoverAChangedParameter(t *testing.T) {
	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.AllowAIHeavy = true })
	ai := domain.Principal{Kind: domain.PrincipalAI, Name: "hermes"}
	if _, err := r.m.Grant(r.ctx(), GrantRequest{
		OpID: "album.scan", For: ai, Target: "photos", Params: "collection=photos limit=10",
		GrantedBy: domain.Principal{Kind: domain.PrincipalUser, Name: "op"},
		ExpiresAt: r.now.Add(time.Hour), OneShot: true,
	}); err != nil {
		t.Fatal(err)
	}

	// The same parameters succeed.
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: ai, OpID: "album.scan", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 10),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true, Verified: true}, nil
		},
	})
	if out.Err != nil {
		t.Fatalf("the approved call failed: %v", out.Err)
	}

	// A larger limit does not.
	out = r.m.Run(r.ctx(), RunOptions{
		Principal: ai, OpID: "album.scan", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 5000),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true}, nil
		},
	})
	if !errors.Is(out.Err, ErrNeedsApproval) {
		t.Errorf("a changed limit was covered by the old approval: error = %v", out.Err)
	}
}

// TestALocalOperatorNeedsNoApproval: requiring the operator to approve their own
// keystroke would make the console unusable without making it safer.
func TestALocalOperatorNeedsNoApproval(t *testing.T) {
	r := newRig(t, nil)
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalLocal, Name: "op"},
		OpID:      "album.scan", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 10),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true, Verified: true}, nil
		},
	})
	if out.Err != nil {
		t.Fatalf("a local operator needed an approval for bounded work: %v", out.Err)
	}
	if out.Job == nil || out.Job.State != domain.JobCompleted {
		t.Fatalf("job = %+v, want completed", out.Job)
	}
}

// TestAUserNeedsAnApprovalForMutatingWork: a human through the API may do bounded
// work directly, but not something that changes files.
func TestAUserNeedsAnApprovalForMutatingWork(t *testing.T) {
	r := newRig(t, nil)
	u := domain.Principal{Kind: domain.PrincipalUser, Name: "op"}
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: u, OpID: "files.mkdir", Target: "data/new",
		Args: operations.NewArgs().Str("root", "data").Str("path", "new"),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true}, nil
		},
	})
	if !errors.Is(out.Err, ErrNeedsApproval) {
		t.Errorf("a mutating call from a user ran without approval: %v", out.Err)
	}
}

// TestAMutexRefusesASecondHeavyRun: two scans at once on this machine is the
// situation the lock exists to make impossible.
func TestAMutexRefusesASecondHeavyRun(t *testing.T) {
	r := newRig(t, nil)
	p := domain.Principal{Kind: domain.PrincipalLocal, Name: "op"}
	o := op(t, "album.scan")

	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan Outcome, 1)
	go func() {
		done <- r.m.Run(context.Background(), RunOptions{
			Principal: p, OpID: "album.scan", Target: "photos",
			Args: operations.NewArgs().Str("collection", "photos").Int("limit", 10),
			Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (Result, error) {
				close(started)
				<-release
				return Result{Completed: true}, nil
			},
		})
	}()
	<-started

	// While the first holds the scan lock, a second is refused at admission.
	d := r.m.Admit(r.ctx(), p, o, operations.NewArgs().Str("collection", "photos").Int("limit", 5))
	if d.Allowed {
		t.Error("a second scan was admitted while one held the lock")
	}
	if d.Gate != GateMutex {
		t.Errorf("gate = %q, want %q", d.Gate, GateMutex)
	}

	close(release)
	if out := <-done; out.Err != nil {
		t.Fatalf("the first scan failed: %v", out.Err)
	}
	// After it finishes the lock is free again.
	if d := r.m.Admit(r.ctx(), p, o, operations.NewArgs().Str("collection", "photos").Int("limit", 5)); !d.Allowed {
		t.Errorf("the lock was not released: %s", d.Detail)
	}
}

// TestAStoppedRunIsNotRecordedAsCompleted: a handler that returns without
// finishing must not be filed as success. This is the difference between "the
// batch reached its limit" and "the batch did its job".
func TestAStoppedRunIsNotRecordedAsCompleted(t *testing.T) {
	r := newRig(t, nil)
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalLocal, Name: "op"},
		OpID:      "album.thumbs", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 5),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			// Stopped for a resource reason, without an error.
			return Result{Completed: false, StopReason: "mem_below_stop",
				Detail: "MemAvailable fell below the stop threshold"}, nil
		},
	})
	if out.Job.State != domain.JobFailed {
		t.Errorf("state = %q, want failed: a run that stopped did not complete", out.Job.State)
	}
	if out.Job.StopReason != "mem_below_stop" {
		t.Errorf("stop reason = %q, want it preserved", out.Job.StopReason)
	}
	if out.Job.Verified {
		t.Error("a stopped run is marked verified")
	}
}

// TestAFailedHandlerRecordsTheError: the diagnostic has to survive into the row,
// or an operator has nothing to act on.
func TestAFailedHandlerRecordsTheError(t *testing.T) {
	r := newRig(t, nil)
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalLocal, Name: "op"},
		OpID:      "album.thumbs", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 5),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{}, fmt.Errorf("decoder refused the file")
		},
	})
	if out.Job.State != domain.JobFailed {
		t.Errorf("state = %q, want failed", out.Job.State)
	}
	if out.Job.Err == "" {
		t.Error("the failure carries no error text")
	}
}

// TestRefusalsAreAudited: a refusal with no trace is indistinguishable from a call
// that never happened, which is what an investigation needs to tell apart.
func TestRefusalsAreAudited(t *testing.T) {
	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.ReadOnlyConsole = true })
	r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalUser, Name: "op"},
		OpID:      "files.mkdir", Target: "data/x",
		Args: operations.NewArgs().Str("root", "data").Str("path", "x"),
		Handler: func(context.Context, *hubstore.Job, *operations.Args) (Result, error) {
			return Result{Completed: true}, nil
		},
	})
	last := r.lastAudit()
	if last.Outcome != "refused" {
		t.Fatalf("the refusal was not audited: %+v", last)
	}
	if last.Op != "files.mkdir" || last.Principal != "user:op" {
		t.Errorf("audit row lost fields: %+v", last)
	}
	if last.Note == "" {
		t.Error("the refusal has no reason recorded")
	}
}

// TestAnUnknownOperationIsRefused: there is no operation with no declared risk.
func TestAnUnknownOperationIsRefused(t *testing.T) {
	r := newRig(t, nil)
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalLocal},
		OpID:      "system.shell", Args: operations.NewArgs(),
	})
	if !errors.Is(out.Err, ErrUnknownOp) {
		t.Errorf("error = %v, want ErrUnknownOp", out.Err)
	}
	if last := r.lastAudit(); last.Outcome != "refused" {
		t.Errorf("an unknown operation was not audited: %+v", last)
	}
}

// TestAnUnimplementedOperationFailsClosed: a declared operation with no handler
// must fail, never silently succeed.
func TestAnUnimplementedOperationFailsClosed(t *testing.T) {
	r := newRig(t, nil)
	out := r.m.Run(r.ctx(), RunOptions{
		Principal: domain.Principal{Kind: domain.PrincipalLocal},
		OpID:      "album.scan", Target: "photos",
		Args: operations.NewArgs().Str("collection", "photos").Int("limit", 10),
		// no handler
	})
	if out.Err == nil {
		t.Fatal("an operation with no handler reported success")
	}
	if out.Job == nil || out.Job.State != domain.JobFailed {
		t.Errorf("job = %+v, want failed", out.Job)
	}
}

// TestTheJobCapIsGated: the queue must not grow past its bound.
func TestTheJobCapIsGated(t *testing.T) {
	r := newRig(t, func(_ *Config, l *hubstore.Limits) { l.MaxOpenJobs = 2 })
	p := domain.Principal{Kind: domain.PrincipalLocal}
	// Two jobs that never finish.
	for i := 0; i < 2; i++ {
		_, err := r.store.CreateJob(r.ctx(), hubstore.NewJob{
			ID: fmt.Sprintf("x%d", i), Op: "album.scan", Principal: "local",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	d := r.m.Admit(r.ctx(), p, op(t, "album.thumbs"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 5))
	if d.Allowed {
		t.Fatal("work was admitted with the job cap already reached")
	}
	if d.Gate != GateJobCap {
		t.Errorf("gate = %q, want %q", d.Gate, GateJobCap)
	}
}

// TestAJobIDIsNotGuessable: sequential IDs would let a caller enumerate other
// callers' jobs.
func TestAJobIDIsNotGuessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewJobID()
		if seen[id] {
			t.Fatalf("NewJobID returned %q twice", id)
		}
		seen[id] = true
		if len(id) < 12 {
			t.Fatalf("job ID %q is too short to be unguessable", id)
		}
	}
}

// TestModesAreAuditedAndValidated: setting a mode is a state change an operator
// will later need to account for.
func TestModesAreAuditedAndValidated(t *testing.T) {
	r := newRig(t, nil)
	p := domain.Principal{Kind: domain.PrincipalUser, Name: "op"}

	if err := r.m.StartMode(r.ctx(), domain.ModeState{Mode: domain.Mode("nonsense")}, p); err == nil {
		t.Error("an invented mode was accepted")
	}
	if err := r.m.StartMode(r.ctx(), domain.ModeState{Mode: domain.ModeNormal}, p); err == nil {
		t.Error("setting 'normal' was accepted; normal is the absence of a mode")
	}
	if err := r.m.StartMode(r.ctx(), domain.ModeState{
		Mode: domain.ModeWatching, ExpiresAt: r.now.Add(-time.Hour),
	}, p); err == nil {
		t.Error("a window that expires in the past was accepted, so the pause would be inert")
	}

	if err := r.m.StartMode(r.ctx(), domain.ModeState{
		Mode: domain.ModeWatching, Reason: "watching a film", ExpiresAt: r.now.Add(2 * time.Hour),
	}, p); err != nil {
		t.Fatalf("a valid window was refused: %v", err)
	}
	if last := r.lastAudit(); last.Action != "mode.set" || last.Target != "watching" {
		t.Errorf("mode set was not audited: %+v", last)
	}

	// resource_guard is not cleared by hand.
	if err := r.m.ClearMode(r.ctx(), domain.ModeResourceGuard, p); err == nil {
		t.Error("an operator cleared the machine's resource guard by hand")
	}
	if err := r.m.ClearMode(r.ctx(), domain.ModeWatching, p); err != nil {
		t.Errorf("clearing a person's own window failed: %v", err)
	}
}

// TestHeldLocksAreObservable: the status page reports what is locked, so the
// lock state must be readable without side effects.
func TestHeldLocksAreObservable(t *testing.T) {
	r := newRig(t, nil)
	if err := r.m.Lock(operations.MutexScan, "job-a"); err != nil {
		t.Fatal(err)
	}
	held := r.m.HeldLocks()
	if held[operations.MutexScan] != "job-a" {
		t.Errorf("held locks = %v, want scan held by job-a", held)
	}
	// A different job cannot release it.
	r.m.Unlock(operations.MutexScan, "job-b")
	if r.m.HeldLocks()[operations.MutexScan] != "job-a" {
		t.Error("one job released another job's lock")
	}
	r.m.Unlock(operations.MutexScan, "job-a")
	if _, still := r.m.HeldLocks()[operations.MutexScan]; still {
		t.Error("the holder could not release its own lock")
	}
}

// TestReconcileRunsOnce: a second call must not rewrite history, or a repeated
// startup would keep relabelling jobs.
func TestReconcileRunsOnce(t *testing.T) {
	r := newRig(t, nil)
	if _, err := r.store.CreateJob(r.ctx(), hubstore.NewJob{
		ID: "j", Op: "album.scan", Principal: "local",
	}); err != nil {
		t.Fatal(err)
	}
	n, _, err := r.m.Reconcile(r.ctx(), func(int, int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("first reconcile = %d, want 1", n)
	}
	n, _, err = r.m.Reconcile(r.ctx(), func(int, int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("second reconcile = %d, want 0", n)
	}
}

// TestAGuardRefusalBlocksAdmission: when the machine is short of memory, the
// gate must close. A guard is wired in here with a deliberately impossible
// budget so the refusal is deterministic.
func TestAGuardRefusalBlocksAdmission(t *testing.T) {
	dir := t.TempDir()
	// A meminfo fixture reporting almost no available memory.
	meminfo := filepath.Join(dir, "meminfo")
	writeFile(t, meminfo, "MemTotal:         1000000 kB\nMemAvailable:       10000 kB\n")
	sampler := resource.NewSampler("/proc", "")
	sampler.MemInfoPath = meminfo

	guard := resource.NewGuard(resource.Limits{
		StartMemAvailableKB: 400 * 1024,
		WarnMemAvailableKB:  250 * 1024,
		StopMemAvailableKB:  200 * 1024,
	}, sampler)

	r := newRig(t, func(c *Config, _ *hubstore.Limits) { c.Guard = guard })
	d := r.m.Admit(r.ctx(), domain.Principal{Kind: domain.PrincipalLocal}, op(t, "album.scan"),
		operations.NewArgs().Str("collection", "photos").Int("limit", 10))
	if d.Allowed {
		t.Fatal("work was admitted while the machine was below its start budget")
	}
	if d.Gate != GateResource {
		t.Errorf("gate = %q, want %q", d.Gate, GateResource)
	}
	if d.Reason == "" {
		t.Error("the refusal names no resource reason")
	}
}

// writeFile is a small helper for fixture files.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
