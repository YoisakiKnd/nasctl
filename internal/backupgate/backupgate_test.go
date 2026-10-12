package backupgate

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// The gate decides whether albumctl yields to the existing backup. Two failure
// modes matter, and they point in opposite directions:
//
//   - Treating an *unanswered* question as "the backup is idle" would let
//     albumctl add its own I/O on top of an upload that is already saturating
//     the box.
//   - Treating an *unanswerable* question as "busy" would stop the album forever
//     because a status script was renamed - and the design requires that the
//     album is always the one that yields, never the backup that gets touched.
//
// The tests below pin both, plus the rule that nothing in this package ever
// signals or reconfigures the backup.

// recordingRunner returns a fixed exit code and records what it was asked to
// run.
type recordingRunner struct {
	mu       sync.Mutex
	code     int
	err      error
	calls    []string
	deadline time.Duration // how long the context allowed, as measured inside
	block    bool          // if set, wait for the context to expire
}

func (r *recordingRunner) run(ctx context.Context, name string, args ...string) (int, error) {
	// Every field is read and written under the mutex. The deadline assignment
	// used to happen before the lock, which the race detector flagged the first
	// time this package was tested with -race: the fixture meant to prove the
	// gate is concurrency-safe was itself racy, so it could not prove anything
	// about the gate.
	r.mu.Lock()
	if dl, has := ctx.Deadline(); has {
		r.deadline = time.Until(dl)
	}
	r.calls = append(r.calls, name)
	for _, a := range args {
		r.calls = append(r.calls, a)
	}
	code, err, block := r.code, r.err, r.block
	r.mu.Unlock()

	if block {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	return code, err
}

func (r *recordingRunner) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func TestNoConfiguredCheckIsReportedAsUnknownNotIdle(t *testing.T) {
	g := New(Config{})
	v := g.Check(context.Background())
	if v.Known {
		t.Error("Known = true with no check configured: an operator would be told the backup " +
			"is idle when the question was never put")
	}
	if v.Busy {
		t.Error("Busy = true with no check configured; nothing is known about the backup")
	}
	if v.Detail == "" {
		t.Error("the verdict must explain itself")
	}
}

func TestAZeroExitCodeMeansBusyByDefault(t *testing.T) {
	// The shipped convention: the status command exits 0 when it is safe to
	// proceed, which for "is the backup busy" is the opposite polarity. The
	// default is therefore "0 means busy", and it is the safe direction.
	g := New(Config{CheckCommand: "backupctl"})
	r := &recordingRunner{code: 0}
	g.Runner = r.run

	v := g.Check(context.Background())
	if !v.Busy || !v.Known {
		t.Fatalf("exit 0 with the shipped busy-code list: Busy=%v Known=%v, want true/true", v.Busy, v.Known)
	}
	if len(r.recorded()) != 1 {
		t.Errorf("the runner was called as %v, want just the command", r.recorded())
	}
}

func TestANonBusyExitCodeMeansIdle(t *testing.T) {
	g := New(Config{CheckCommand: "backupctl", BusyExitCodes: []int{0}})
	g.Runner = (&recordingRunner{code: 3}).run

	v := g.Check(context.Background())
	if v.Busy {
		t.Error("exit 3 is not in the busy list")
	}
	if !v.Known {
		t.Error("an answered check must be Known")
	}
}

func TestCustomBusyExitCodesAreHonoured(t *testing.T) {
	g := New(Config{CheckCommand: "backupctl", BusyExitCodes: []int{7, 9}})

	g.Runner = (&recordingRunner{code: 7}).run
	if v := g.Check(context.Background()); !v.Busy {
		t.Error("exit 7 is in the busy list")
	}
	g.Runner = (&recordingRunner{code: 9}).run
	if v := g.Check(context.Background()); !v.Busy {
		t.Error("exit 9 is in the busy list")
	}
	g.Runner = (&recordingRunner{code: 0}).run
	if v := g.Check(context.Background()); v.Busy {
		t.Error("exit 0 is not in this busy list, and 0 must not be hard-coded as busy")
	}
}

func TestAStatusScriptThatHangsCountsAsBusy(t *testing.T) {
	// An unresponsive status script is itself a sign of a loaded machine, so the
	// answer that keeps albumctl out of the way is the safe one.
	g := New(Config{CheckCommand: "backupctl", CheckTimeout: 100 * time.Millisecond})
	g.Runner = (&recordingRunner{block: true}).run

	start := time.Now()
	v := g.Check(context.Background())
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("the check took %s; the timeout was not applied", elapsed)
	}
	if !v.Busy || !v.Known {
		t.Fatalf("a hanging check: Busy=%v Known=%v, want true/true", v.Busy, v.Known)
	}
	if v.Detail == "" {
		t.Error("the verdict must say that the check did not answer in time")
	}
}

func TestACheckThatCannotBeRunIsUnknownNotIdle(t *testing.T) {
	// A renamed or missing status command must not be read as "the backup is
	// idle": that is how albumctl would end up racing an upload.
	g := New(Config{CheckCommand: "backupctl-does-not-exist"})
	g.Runner = func(ctx context.Context, name string, args ...string) (int, error) {
		return -1, exec.ErrNotFound
	}

	v := g.Check(context.Background())
	if v.Busy {
		t.Error("a check that could not run cannot prove the backup is busy")
	}
	if v.Known {
		t.Error("a check that could not run cannot prove the backup is idle either: it must be " +
			"reported as unknown")
	}
}

func TestTheRealRunnerReadsAnExitCodeFromARealStatusCommand(t *testing.T) {
	// These cases run a real subprocess through the default runner, because the
	// part being tested is precisely the boundary between the operating system
	// and the verdict: a status script that reports "busy" by exiting non-zero
	// arrives as an *exec.ExitError, and the code inside it is the answer, not a
	// failure to ask.
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}

	busy := New(Config{
		CheckCommand:  "/bin/sh",
		CheckArgs:     []string{"-c", "exit 2"},
		BusyExitCodes: []int{2},
	})
	if v := busy.Check(context.Background()); !v.Busy || !v.Known {
		t.Errorf("a real script exiting 2 with 2 in the busy list: Busy=%v Known=%v, want true/true",
			v.Busy, v.Known)
	}

	idle := New(Config{
		CheckCommand:  "/bin/sh",
		CheckArgs:     []string{"-c", "exit 2"},
		BusyExitCodes: []int{0},
	})
	if v := idle.Check(context.Background()); v.Busy || !v.Known {
		t.Errorf("a real script exiting 2 with only 0 in the busy list: Busy=%v Known=%v, "+
			"want false/true", v.Busy, v.Known)
	}

	ok := New(Config{
		CheckCommand:  "/bin/sh",
		CheckArgs:     []string{"-c", "exit 0"},
		BusyExitCodes: []int{0},
	})
	if v := ok.Check(context.Background()); !v.Busy || !v.Known {
		t.Errorf("a real script exiting 0 with the shipped polarity: Busy=%v Known=%v, "+
			"want true/true", v.Busy, v.Known)
	}

	missing := New(Config{CheckCommand: "/nonexistent/backupctl"})
	if v := missing.Check(context.Background()); v.Busy || v.Known {
		t.Errorf("a command that does not exist: Busy=%v Known=%v, want false/false - it must "+
			"not be read as idle", v.Busy, v.Known)
	}
}

func TestArgumentsArePassedWithoutAShell(t *testing.T) {
	// CheckArgs are not word-split, so a path with a space cannot become two
	// arguments and cannot be turned into a second command by a shell.
	args := []string{"--state", "/var/lib/backup dir", "--json"}
	g := New(Config{CheckCommand: "backupctl", CheckArgs: args})
	r := &recordingRunner{code: 3}
	g.Runner = r.run

	g.Check(context.Background())
	got := r.recorded()
	if len(got) != 1+len(args) {
		t.Fatalf("runner saw %v, want the command plus %d arguments", got, len(args))
	}
	if got[2] != "/var/lib/backup dir" {
		t.Errorf("argument was split or rewritten: %q", got[2])
	}
}

func TestTheCheckTimeoutIsAlwaysBounded(t *testing.T) {
	// A status script must never be able to hang an album task, including when
	// the configuration asks for an absurd timeout.
	for _, tc := range []struct {
		name  string
		cfg   Config
		limit time.Duration
	}{
		{"unset", Config{CheckCommand: "x"}, 5 * time.Second},
		{"absurd", Config{CheckCommand: "x", CheckTimeout: 10 * time.Minute}, 60 * time.Second},
		{"negative", Config{CheckCommand: "x", CheckTimeout: -time.Second}, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := New(tc.cfg)
			r := &recordingRunner{code: 3}
			g.Runner = r.run
			g.Check(context.Background())
			if r.deadline <= 0 || r.deadline > tc.limit {
				t.Errorf("the runner got a deadline of %s, want (0, %s]", r.deadline, tc.limit)
			}
		})
	}
}

func TestSystemdUnitsAreQueriedOnlyWhenNoCommandIsConfigured(t *testing.T) {
	g := New(Config{CheckCommand: "backupctl", SystemdUnits: []string{"rclone.service"}})
	r := &recordingRunner{code: 3}
	g.Runner = r.run
	g.Check(context.Background())
	if got := r.recorded(); len(got) != 1 || got[0] != "backupctl" {
		t.Fatalf("with a command configured the units must not be queried, got %v", got)
	}

	units := New(Config{SystemdUnits: []string{"rclone.service"}})
	r2 := &recordingRunner{code: 0}
	units.Runner = r2.run
	if v := units.Check(context.Background()); !v.Busy {
		t.Error("an active unit must count as busy")
	}
	got := r2.recorded()
	if len(got) != 4 || got[0] != "systemctl" || got[3] != "rclone.service" {
		t.Errorf("systemctl was called as %v, want is-active --quiet <unit>", got)
	}
}

func TestAnInactiveUnitIsIdleAndAMixedAnswerIsBusy(t *testing.T) {
	units := New(Config{SystemdUnits: []string{"a.service", "b.service"}})
	var mu sync.Mutex
	answers := map[string]int{"a.service": 3, "b.service": 3}
	units.Runner = func(ctx context.Context, name string, args ...string) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return answers[args[len(args)-1]], nil
	}

	if v := units.Check(context.Background()); v.Busy || !v.Known {
		t.Fatalf("all units inactive: Busy=%v Known=%v, want false/true", v.Busy, v.Known)
	}

	mu.Lock()
	answers["b.service"] = 0
	mu.Unlock()
	if v := units.Check(context.Background()); !v.Busy {
		t.Error("one active unit is enough to make the answer busy: the backup must be assumed " +
			"to be using the machine")
	}
}

func TestAllUnknownUnitsAreReportedAsUnknown(t *testing.T) {
	units := New(Config{SystemdUnits: []string{"a.service"}})
	units.Runner = func(ctx context.Context, name string, args ...string) (int, error) {
		return -1, exec.ErrNotFound
	}
	if v := units.Check(context.Background()); v.Known || v.Busy {
		t.Errorf("Busy=%v Known=%v, want false/false when systemctl cannot be run", v.Busy, v.Known)
	}
}

func TestConfiguredTellsTheDifferenceBetweenIdleAndUnasked(t *testing.T) {
	if (Config{}).Configured() {
		t.Error("an empty configuration has no check")
	}
	if (Config{CheckCommand: "  "}).Configured() {
		t.Error("a blank command is not a configured check")
	}
	if !(Config{CheckCommand: "backupctl"}).Configured() {
		t.Error("a command is a configured check")
	}
	if !(Config{SystemdUnits: []string{"rclone.service"}}).Configured() {
		t.Error("units are a configured check")
	}
}

func TestSanitizeClampsTheTimeout(t *testing.T) {
	d := Defaults()
	if d.CheckTimeout != 5*time.Second {
		t.Errorf("the shipped timeout is %s, want 5s", d.CheckTimeout)
	}
	if d.SkipThumbsWhenBusy != true {
		t.Error("the shipped policy must make the thumbnail task yield by default")
	}
	if got := (Config{CheckTimeout: 10 * time.Minute}).Sanitize().CheckTimeout; got != 60*time.Second {
		t.Errorf("Sanitize left the timeout at %s; it must be clamped to 60s", got)
	}
	if got := (Config{CheckTimeout: -1}).Sanitize().CheckTimeout; got != 5*time.Second {
		t.Errorf("Sanitize turned a negative timeout into %s, want the 5s default", got)
	}
}

func TestTheGateIsSafeForConcurrentUse(t *testing.T) {
	// The web service and a task can both ask, so the gate is documented as
	// concurrency-safe. Run under the race detector, this catches shared state.
	g := New(Config{CheckCommand: "backupctl"})
	g.Runner = (&recordingRunner{code: 3}).run

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if v := g.Check(context.Background()); v.Busy {
					t.Error("exit 3 is idle for this configuration")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestTheGateNeverRunsAnythingButTheStatusCommand(t *testing.T) {
	// The design rule: albumctl yields, always. Nothing in this package stops,
	// signals or reconfigures the backup, so the only command it may ever run is
	// the configured status check.
	g := New(Config{CheckCommand: "backupctl", CheckArgs: []string{"status"}})
	r := &recordingRunner{code: 3}
	g.Runner = r.run
	g.Check(context.Background())

	for i, c := range r.recorded() {
		if i%2 == 0 && c != "backupctl" {
			t.Errorf("the gate ran %q; it may only run the configured status command", c)
		}
	}
}
