package domain

import (
	"strings"
	"testing"
	"time"
)

func TestModePrecedencePutsSafetyFirst(t *testing.T) {
	now := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)

	cases := []struct {
		name   string
		set    ModeSet
		want   Mode
		reason string
	}{
		{
			name:   "nothing set is normal",
			set:    ModeSet{},
			want:   ModeNormal,
			reason: "an empty mode set must not look like a restriction",
		},
		{
			name: "a human pause beats an automatic watching window",
			set: ModeSet{States: []ModeState{
				{Mode: ModeWatching, Source: "user:a", ExpiresAt: later},
				{Mode: ModeManualPause, Source: "user:b"},
			}},
			want:   ModeManualPause,
			reason: "automation must never lift a human pause",
		},
		{
			name: "a resource trip beats a human pause",
			set: ModeSet{States: []ModeState{
				{Mode: ModeManualPause, Source: "user:b"},
				{Mode: ModeResourceGuard, Source: "system"},
			}},
			want: ModeResourceGuard,
			reason: "the machine saying stop outranks a person saying stop, because the " +
				"consequence of ignoring it is the machine going away",
		},
		{
			name: "maintenance does not override watching",
			set: ModeSet{States: []ModeState{
				{Mode: ModeMaintenance, Source: "user:a", ExpiresAt: later},
				{Mode: ModeWatching, Source: "user:b", ExpiresAt: later},
			}},
			want: ModeWatching,
			reason: "restriction wins every tie: a maintenance window must not license " +
				"heavy work while somebody is watching",
		},
		{
			name: "an expired window stops applying",
			set: ModeSet{States: []ModeState{
				{Mode: ModeWatching, Source: "user:a", ExpiresAt: now.Add(-time.Minute)},
			}},
			want: ModeNormal,
			reason: "a forgotten window must expire on its own rather than stop " +
				"maintenance forever",
		},
		{
			name: "a durable pause with no expiry still applies",
			set: ModeSet{States: []ModeState{
				{Mode: ModeManualPause, Source: "user:a"},
			}},
			want:   ModeManualPause,
			reason: "a zero expiry means durable, not expired",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.set.Effective(now)
			if got.Mode != c.want {
				t.Errorf("Effective() = %q, want %q (%s)", got.Mode, c.want, c.reason)
			}
		})
	}
}

func TestOnlyNormalAndMaintenanceAllowHeavyWork(t *testing.T) {
	now := time.Now()
	for _, m := range []Mode{ModeNormal, ModeMaintenance, ModeWatching, ModeManualPause, ModeResourceGuard} {
		st := ModeState{Mode: m, ExpiresAt: now.Add(time.Hour)}
		want := m == ModeNormal || m == ModeMaintenance
		if got := st.AllowsHeavyWork(); got != want {
			t.Errorf("mode %q AllowsHeavyWork() = %v, want %v", m, got, want)
		}
	}
}

func TestJobStateTransitions(t *testing.T) {
	legal := []struct{ from, to JobState }{
		{JobQueued, JobRunning},
		{JobQueued, JobWaiting},
		{JobQueued, JobCancelled},
		{JobRunning, JobCompleted},
		{JobRunning, JobFailed},
		{JobRunning, JobPaused},
		{JobRunning, JobInterrupted},
		{JobWaiting, JobRunning},
		{JobPaused, JobRunning},
		{JobInterrupted, JobUnknown},
		{JobUnknown, JobFailed},
		// A queued job may fail at admission, before it ever runs. Forcing it
		// through "running" first would record a state it was never in.
		{JobQueued, JobFailed},
	}
	for _, c := range legal {
		if err := c.from.Transition(c.to); err != nil {
			t.Errorf("Transition(%q -> %q) = %v, want nil", c.from, c.to, err)
		}
	}

	illegal := []struct{ from, to JobState }{
		// The important one: a job must not be able to jump from accepted to
		// *completed* without ever running. That is the record shape that makes
		// a console's history untrustworthy: it claims work was done that never
		// started.
		{JobQueued, JobCompleted},
		{JobCompleted, JobRunning},
		{JobFailed, JobCompleted},
		{JobCancelled, JobRunning},
		{JobCompleted, JobQueued},
	}
	for _, c := range illegal {
		if err := c.from.Transition(c.to); err == nil {
			t.Errorf("Transition(%q -> %q) = nil, want a refusal", c.from, c.to)
		}
	}

	if err := JobRunning.Transition(JobState("finished")); err == nil {
		t.Error("an invented state was accepted")
	}
}

func TestJobStatePredicates(t *testing.T) {
	if !JobCompleted.Terminal() || !JobFailed.Terminal() || !JobCancelled.Terminal() {
		t.Error("a final state is not reported as terminal")
	}
	if JobRunning.Terminal() || JobQueued.Terminal() {
		t.Error("an open state is reported as terminal")
	}
	if !JobQueued.Open() {
		t.Error("a queued job does not hold a slot")
	}
	// Interrupted is *not* terminal: the job still holds a slot and still needs
	// reconciliation, which is precisely why it is surfaced rather than filed
	// away as a finished failure.
	if !JobInterrupted.Open() || JobInterrupted.Terminal() {
		t.Error("an interrupted job is treated as finished, so nothing will reconcile it")
	}
	// The states where the console does not know what happened must be
	// surfaced, not smoothed over.
	for _, s := range []JobState{JobFailed, JobInterrupted, JobUnknown} {
		if !s.NeedsAttention() {
			t.Errorf("%q does not need attention", s)
		}
	}
	for _, s := range []JobState{JobRunning, JobCompleted, JobQueued} {
		if s.NeedsAttention() {
			t.Errorf("%q unexpectedly needs attention", s)
		}
	}
}

func TestModeValidity(t *testing.T) {
	for _, m := range []Mode{ModeNormal, ModeWatching, ModeMaintenance, ModeManualPause, ModeResourceGuard} {
		if !m.Valid() {
			t.Errorf("%q is not valid", m)
		}
	}
	if Mode("paused").Valid() {
		t.Error("an invented mode was accepted")
	}
}

func TestPrincipalStringIsStable(t *testing.T) {
	p := Principal{Kind: PrincipalAI, Name: "hermes"}
	if got := p.String(); got != "ai:hermes" {
		t.Errorf("String() = %q, want ai:hermes", got)
	}
	p = Principal{Kind: PrincipalLocal}
	if got := p.String(); got != "local" {
		t.Errorf("String() = %q, want local", got)
	}
}

func TestRedactRemovesCredentialsAndTruncates(t *testing.T) {
	cases := map[string]string{
		"token=abc123":       "[redacted]",
		"my-api-key-here":    "[redacted]",
		"userPassword":       "[redacted]",
		"Bearer xyz":         "[redacted]",
		"photos/2024":        "photos/2024",
		"collection=holiday": "collection=holiday",
	}
	for in, want := range cases {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("a", 500)
	got := Redact(long)
	if len(got) > 210 {
		t.Errorf("Redact left %d characters; audit fields must be bounded", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated value does not show that it was truncated: %q", got[len(got)-5:])
	}
}
