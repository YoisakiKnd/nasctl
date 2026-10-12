package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// Handler does the work of one operation.
//
// It receives the job (so it can report progress and observe cancellation) and
// the validated arguments. Returning an error marks the job failed; returning a
// Result lets the handler state its own outcome, which matters for work that
// stops for a reason that is not a failure - a bounded run that reached its file
// limit completed successfully, and must not be recorded as a failure.
type Handler func(ctx context.Context, job *hubstore.Job, args *operations.Args) (Result, error)

// Result is what a handler reports when it stops.
type Result struct {
	// StopReason is a machine-readable cause, drawn from resource.Reason where
	// one applies.
	StopReason string
	// Completed reports whether the work finished as intended. A run that
	// stopped at its file limit completed; a run that stopped because the
	// machine ran out of memory did not.
	Completed bool
	// Verified reports whether the outcome was confirmed by looking at the
	// result rather than inferred from the absence of errors.
	Verified bool
	// Detail is a short human explanation.
	Detail string
}

// RunOptions describes one call.
type RunOptions struct {
	Principal domain.Principal
	OpID      string
	Args      *operations.Args
	// Target is the thing the operation acts on, in the console's own
	// vocabulary: a storage ID, a collection code, a job ID. It is recorded and
	// is part of an approval's identity.
	Target string
	// Sync, when true, runs the handler inline and returns its result rather
	// than creating a job. It is used for bounded reads that a caller waits for.
	Sync bool
	// Handler does the work.
	Handler Handler
	// OnAccepted, if set, is called with the created job before the handler
	// runs, so a caller can report the job ID immediately.
	OnAccepted func(*hubstore.Job)
}

// Outcome is the result of a Run.
type Outcome struct {
	Job      *hubstore.Job
	Decision Decision
	Result   Result
	Err      error
}

// Run performs one call end to end: admit, record, approve, execute, audit.
//
// Every path through this function writes an audit row, including the refusals.
// A refusal that leaves no trace is indistinguishable from a call that never
// happened, which is exactly what an operator investigating "who tried to do
// that" needs to be able to tell apart.
func (m *Manager) Run(ctx context.Context, o RunOptions) Outcome {
	op, ok := m.cfg.Registry.Lookup(o.OpID)
	if !ok {
		err := fmt.Errorf("%w: %q", ErrUnknownOp, o.OpID)
		m.audit(ctx, hubstore.AuditEntry{
			Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
			Outcome: "refused", Note: err.Error(),
		})
		return Outcome{Err: err}
	}

	d := m.Admit(ctx, o.Principal, op, o.Args)
	if !d.Allowed {
		// Distinguish "you may not" from "not now": the first is a policy
		// refusal the caller should not retry, the second is a condition that
		// may pass.
		err := ErrDenied
		switch d.Gate {
		case GateMode, GateResource, GateMutex, GateJobCap:
			err = fmt.Errorf("%w: %s", ErrGated, d.Detail)
		case GateApproval, GateReadOnly:
			err = fmt.Errorf("%w: %s", ErrDenied, d.Detail)
		}
		m.audit(ctx, hubstore.AuditEntry{
			Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
			Params: o.Args.Summary(op), Outcome: "refused",
			Note: fmt.Sprintf("gate=%s %s", d.Gate, d.Detail),
		})
		return Outcome{Decision: d, Err: err}
	}

	// The approval is consumed here rather than in Admit, because consuming is
	// a write and an admission check must be free of side effects: a caller that
	// checks and then decides not to proceed must not burn the operator's
	// approval.
	params := o.Args.Summary(op)
	var approvalID string
	if op.Approval == domain.ApprovalPerCall || op.Approval == domain.ApprovalPreauthorised {
		ap, err := m.cfg.Store.ConsumeApproval(ctx, op.ID, o.Principal.String(), o.Target, params, m.now())
		if err != nil {
			// A human at the console, or the local operator, may act on their
			// own authority: the approval requirement exists to bind *agents*
			// and remote callers, and requiring an operator to approve their
			// own keystroke would make the console unusable without making it
			// safer.
			if !m.mayActUnapproved(o.Principal, op) {
				err := fmt.Errorf("%w: %s requires an approval for target %q with parameters "+
					"%q; none is valid", ErrNeedsApproval, op.ID, o.Target, params)
				m.audit(ctx, hubstore.AuditEntry{
					Principal: o.Principal.String(), Action: "call", Op: o.OpID,
					Target: o.Target, Params: params, Outcome: "refused",
					Note: err.Error(),
				})
				return Outcome{Decision: d, Err: err}
			}
		} else {
			approvalID = ap.ID
		}
	}

	// A synchronous read does not create a job: it answers from a bounded query
	// and returns. Creating a job row for every file listing would turn the job
	// table into a request log.
	if o.Sync && op.ReadOnly {
		res, err := m.runSync(ctx, o, op)
		return Outcome{Decision: d, Result: res, Err: err}
	}

	job, err := m.cfg.Store.CreateJob(ctx, hubstore.NewJob{
		ID: NewJobID(), Op: op.ID, Principal: o.Principal.String(),
		Params: params, Target: o.Target, ApprovalID: approvalID,
		StartQueued: false,
	})
	if err != nil {
		m.audit(ctx, hubstore.AuditEntry{
			Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
			Params: params, Outcome: "refused", Note: err.Error(),
		})
		return Outcome{Decision: d, Err: err}
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
		Params: params, Outcome: "accepted", Note: "job " + job.ID,
	})
	if o.OnAccepted != nil {
		o.OnAccepted(job)
	}

	// Hold the mutex for the whole run so that a second caller is refused rather
	// than queued behind it. A queued heavy operation on this machine is a
	// promise the console cannot keep.
	if err := m.Lock(op.Mutex, job.ID); err != nil {
		_ = m.cfg.Store.Finish(ctx, job.ID, domain.JobFailed, "mutex_busy", err.Error(), false)
		m.audit(ctx, hubstore.AuditEntry{
			Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
			Outcome: "failed", Note: err.Error(),
		})
		return Outcome{Job: m.refresh(ctx, job), Decision: d, Err: err}
	}
	defer m.Unlock(op.Mutex, job.ID)

	if o.Handler == nil {
		_ = m.cfg.Store.Finish(ctx, job.ID, domain.JobFailed, "no_handler",
			"this operation has no implementation in this build", false)
		err := fmt.Errorf("jobs: operation %s has no handler", op.ID)
		m.audit(ctx, hubstore.AuditEntry{
			Principal: o.Principal.String(), Action: "call", Op: o.OpID,
			Outcome: "failed", Note: err.Error(),
		})
		return Outcome{Job: m.refresh(ctx, job), Decision: d, Err: err}
	}

	res, herr := o.Handler(ctx, job, o.Args)
	final := domain.JobCompleted
	if herr != nil {
		final = domain.JobFailed
	} else if !res.Completed {
		// The handler stopped without finishing and without erroring: that is a
		// stop, not a success. Recording it as completed would be the console
		// claiming work it did not do.
		final = domain.JobFailed
		if res.StopReason == "" {
			res.StopReason = "stopped_without_completing"
		}
	}
	errMsg := ""
	if herr != nil {
		errMsg = herr.Error()
	} else if res.Detail != "" && final == domain.JobFailed {
		errMsg = res.Detail
	}
	if ferr := m.cfg.Store.Finish(ctx, job.ID, final, res.StopReason, errMsg, res.Verified); ferr != nil {
		// The work is done; failing to record it is a bookkeeping fault that the
		// caller must still hear about, because an unrecorded job looks running.
		if herr == nil {
			herr = fmt.Errorf("the work finished but its outcome could not be recorded: %w", ferr)
		}
	}
	outcome := "ok"
	if final == domain.JobFailed {
		outcome = "failed"
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
		Params: params, Outcome: outcome, Note: noteFor(res, errMsg),
	})

	return Outcome{Job: m.refresh(ctx, job), Decision: d, Result: res, Err: herr}
}

// refresh re-reads a job so that the object handed back to a caller matches the
// row that was written. Returning the pre-finish object would tell a caller a
// job is still running when it has already been recorded as failed, which is the
// sort of contradiction that makes a console's own reporting untrustworthy.
func (m *Manager) refresh(ctx context.Context, j *hubstore.Job) *hubstore.Job {
	if j == nil {
		return nil
	}
	if done, err := m.cfg.Store.Job(ctx, j.ID); err == nil {
		return done
	}
	return j
}

// runSync executes a bounded read inline and audits it.
func (m *Manager) runSync(ctx context.Context, o RunOptions, op operations.Operation) (Result, error) {
	if o.Handler == nil {
		return Result{}, fmt.Errorf("jobs: operation %s has no handler", op.ID)
	}
	res, err := o.Handler(ctx, nil, o.Args)
	outcome := "ok"
	note := res.Detail
	if err != nil {
		outcome = "failed"
		note = err.Error()
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: o.Principal.String(), Action: "call", Op: o.OpID, Target: o.Target,
		Params: o.Args.Summary(op), Outcome: outcome, Note: domain.Redact(note),
	})
	return res, err
}

// noteFor renders the audit note for a finished run.
func noteFor(res Result, errMsg string) string {
	switch {
	case errMsg != "":
		return domain.Redact(errMsg)
	case res.StopReason != "":
		return "stopped: " + res.StopReason
	default:
		return domain.Redact(res.Detail)
	}
}

// mayActUnapproved reports whether a principal may perform an operation without
// consuming an approval.
//
// Two cases qualify. The local operator at the machine is the authority that
// grants approvals, so requiring one of them is circular. A human principal
// through the console or API is the same person; the console records who acted
// either way. An AI principal never qualifies, and neither does a helper, which
// must act only on a specific grant.
func (m *Manager) mayActUnapproved(p domain.Principal, op operations.Operation) bool {
	switch p.Kind {
	case domain.PrincipalLocal:
		return true
	case domain.PrincipalUser:
		// A human may do bounded work directly. Anything that changes files or
		// the machine still needs an explicit approval, because those are the
		// actions where "I did not mean to do that" is expensive.
		return op.Class == domain.RiskBounded
	default:
		return false
	}
}

// Grant records an approval. It refuses when the granting principal is not
// entitled to grant, which is how "an AI cannot approve itself" is enforced in
// code rather than in documentation.
func (m *Manager) Grant(ctx context.Context, g GrantRequest) (*hubstore.Approval, error) {
	op, ok := m.cfg.Registry.Lookup(g.OpID)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownOp, g.OpID)
	}
	if op.ReadOnly {
		return nil, fmt.Errorf("jobs: %s reads only and never needs an approval", op.ID)
	}
	if op.Approval == domain.ApprovalNone {
		return nil, fmt.Errorf("jobs: %s requires no approval", op.ID)
	}

	// The granter must be entitled to grant.
	switch g.GrantedBy.Kind {
	case domain.PrincipalLocal, domain.PrincipalUser:
		// Fine: a person is granting.
	case domain.PrincipalAI:
		return nil, fmt.Errorf("%w: an AI principal (%s) cannot grant an approval; "+
			"approved=true and an agent's own assertion are not authority",
			ErrDenied, g.GrantedBy)
	case domain.PrincipalHelper:
		return nil, fmt.Errorf("%w: a helper grants nothing; it executes fixed operations",
			ErrDenied)
	default:
		return nil, fmt.Errorf("%w: principal kind %q cannot grant approvals",
			ErrDenied, g.GrantedBy.Kind)
	}

	// A grant to an AI principal for heavy work is refused unless the operator
	// enabled it, so that the two switches cannot disagree.
	if g.For.Kind == domain.PrincipalAI && !m.cfg.AllowAIHeavy && op.Class != domain.RiskBounded {
		return nil, fmt.Errorf("%w: granting an AI principal %s (class %s) is refused while "+
			"allow_ai_heavy is off", ErrDenied, op.ID, op.Class)
	}

	if g.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("jobs: an approval must have an expiry; one without is a " +
			"permanent permission nobody will remember granting")
	}
	if !g.ExpiresAt.After(m.now()) {
		return nil, fmt.Errorf("jobs: the approval would expire at %s, which is not in the future",
			g.ExpiresAt.Format(time.RFC3339))
	}
	maxLife := 24 * time.Hour
	if op.Class == domain.RiskBounded {
		maxLife = 12 * time.Hour
	}
	if g.ExpiresAt.After(m.now().Add(maxLife)) {
		return nil, fmt.Errorf("jobs: an approval for %s may not last longer than %s",
			op.ID, maxLife)
	}

	// The target must be something the operation can actually act on, so that
	// "approve everything" cannot be expressed by leaving it blank when the
	// operation needs a target.
	if g.Target == "" && len(op.Scope) > 0 {
		return nil, fmt.Errorf("jobs: %s acts on a target, so an approval for it must name one",
			op.ID)
	}

	a := hubstore.Approval{
		ID:        NewApprovalID(),
		Op:        op.ID,
		Principal: g.For.String(),
		Target:    g.Target,
		Params:    g.Params,
		GrantedBy: g.GrantedBy.String(),
		GrantedAt: m.now(),
		ExpiresAt: g.ExpiresAt,
		OneShot:   g.OneShot,
	}
	if err := m.cfg.Store.GrantApproval(ctx, a); err != nil {
		return nil, err
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: g.GrantedBy.String(), Action: "approval.grant", Op: op.ID,
		Target: g.Target, Params: domain.Redact(g.Params), Outcome: "ok",
		Note: fmt.Sprintf("granted to %s until %s", g.For, g.ExpiresAt.Format(time.RFC3339)),
	})
	return &a, nil
}

// GrantRequest describes an approval to record.
type GrantRequest struct {
	OpID      string
	For       domain.Principal
	Target    string
	Params    string
	GrantedBy domain.Principal
	ExpiresAt time.Time
	OneShot   bool
}

// Revoke removes an approval.
func (m *Manager) Revoke(ctx context.Context, id string, by domain.Principal) error {
	if err := m.cfg.Store.RevokeApproval(ctx, id); err != nil {
		return err
	}
	m.audit(ctx, hubstore.AuditEntry{
		Principal: by.String(), Action: "approval.revoke", Target: id, Outcome: "ok",
	})
	return nil
}

// ErrNoHandler reports an operation that is declared but not implemented.
var ErrNoHandler = errors.New("jobs: operation has no handler")
