// Package backupgate answers one question: is the existing backup busy right
// now, so that albumctl should stay out of the way?
//
// It deliberately does not try to be clever. The design requires that the
// answer come from an existing interface - the operator's own backupctl script,
// or a systemd unit's state - rather than from guessing at process names. A
// name-matching heuristic is wrong in both directions: it misses a backup that
// was renamed, and it stops albumctl because an unrelated process happens to
// contain the substring "rclone".
//
// Nothing in this package ever stops, signals or reconfigures the backup. It
// only reads a status and reports it. The backup keeps running whatever
// albumctl decides; albumctl is always the one that yields.
package backupgate

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Verdict is the outcome of one check.
type Verdict struct {
	Busy   bool
	Detail string

	// Known is false when no check is configured at all. This is important to
	// surface: "not busy" and "nobody asked" are different answers, and an
	// operator should not be told the backup is idle when the question was
	// never put.
	Known bool
}

// Config is the [backup] section.
type Config struct {
	// CheckCommand is the preferred interface: a command whose exit code
	// answers the question. It should be the existing backupctl or an
	// equivalent status script.
	CheckCommand string
	// CheckArgs are passed to CheckCommand. They are separate from the command
	// so no shell is involved: the string is never word-split, and a path with
	// a space in it cannot become two arguments.
	CheckArgs []string
	// BusyExitCodes are the exit codes that mean "the backup is busy". The
	// default is {0}, matching the convention that a status command exits 0
	// when it is safe to proceed... which is the opposite. See below.
	BusyExitCodes []int
	// CheckTimeout bounds the check. A status script that hangs must not hang
	// the album task.
	CheckTimeout time.Duration
	// SystemdUnits are queried with `systemctl is-active` when no command is
	// configured. Their states are "active" or "activating" when busy.
	SystemdUnits []string
	// SkipThumbsWhenBusy makes the thumbnail task yield. It is on by default.
	SkipThumbsWhenBusy bool
	// RequireKnown makes the task refuse to run when no check is configured.
	// Off by default, so a machine with no backup interface still works.
	RequireKnown bool
}

// Defaults returns the shipped policy.
func Defaults() Config {
	return Config{
		CheckTimeout:       5 * time.Second,
		BusyExitCodes:      []int{0},
		SkipThumbsWhenBusy: true,
	}
}

// Sanitize fills in the zero values.
func (c Config) Sanitize() Config {
	if c.CheckTimeout <= 0 {
		c.CheckTimeout = 5 * time.Second
	}
	if c.CheckTimeout > 60*time.Second {
		c.CheckTimeout = 60 * time.Second
	}
	return c
}

// Configured reports whether any check exists.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.CheckCommand) != "" || len(c.SystemdUnits) > 0
}

// Gate performs checks. It is safe for concurrent use.
type Gate struct {
	cfg Config

	// Runner executes the check command. It is a field so tests can observe
	// what would be run without running it, and so an embedding program can
	// replace it.
	Runner func(ctx context.Context, name string, args ...string) (int, error)
}

// New builds a gate.
func New(cfg Config) *Gate {
	return &Gate{cfg: cfg.Sanitize()}
}

// Check reports whether the backup is busy.
//
// It never returns an error: a check that cannot run is reported as unknown,
// because a broken status script must not be able to block the album forever
// nor be silently mistaken for "idle".
func (g *Gate) Check(ctx context.Context) Verdict {
	if !g.cfg.Configured() {
		return Verdict{Known: false, Detail: "no backup check is configured"}
	}

	if cmd := strings.TrimSpace(g.cfg.CheckCommand); cmd != "" {
		return g.runCommand(ctx, cmd)
	}
	return g.checkUnits(ctx)
}

func (g *Gate) runCommand(ctx context.Context, name string) Verdict {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.CheckTimeout)
	defer cancel()

	run := g.Runner
	if run == nil {
		run = defaultRunner
	}
	code, err := run(ctx, name, g.cfg.CheckArgs...)

	busyCodes := g.cfg.BusyExitCodes
	if len(busyCodes) == 0 {
		busyCodes = []int{0}
	}

	if err != nil {
		// A timeout is a distinct and common case worth naming precisely: the
		// backup script being slow is itself a sign the machine is loaded.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Verdict{
				Busy:   true,
				Known:  true,
				Detail: fmt.Sprintf("%s did not answer within %s; treating an unresponsive status check as busy", name, g.cfg.CheckTimeout),
			}
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return Verdict{
				Known:  false,
				Detail: fmt.Sprintf("the backup check %s could not be run: %v", name, err),
			}
		}
	}

	for _, b := range busyCodes {
		if code == b {
			return Verdict{
				Busy:   true,
				Known:  true,
				Detail: fmt.Sprintf("the backup check %s reported busy (exit %d)", name, code),
			}
		}
	}
	return Verdict{
		Known:  true,
		Detail: fmt.Sprintf("the backup check %s reported idle (exit %d)", name, code),
	}
}

func (g *Gate) checkUnits(ctx context.Context) Verdict {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.CheckTimeout)
	defer cancel()

	run := g.Runner
	if run == nil {
		run = defaultRunner
	}
	var busy, idle, unknown []string
	for _, u := range g.cfg.SystemdUnits {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		// is-active exits 0 for active/activating, 3 for inactive/failed.
		code, err := run(ctx, "systemctl", "is-active", "--quiet", u)
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				unknown = append(unknown, u)
				continue
			}
		}
		if code == 0 {
			busy = append(busy, u)
		} else {
			idle = append(idle, u)
		}
	}
	if len(busy) > 0 {
		return Verdict{
			Busy:   true,
			Known:  true,
			Detail: "these units are active: " + strings.Join(busy, ", "),
		}
	}
	if len(unknown) > 0 && len(idle) == 0 {
		return Verdict{Known: false, Detail: "systemctl could not be queried for: " + strings.Join(unknown, ", ")}
	}
	return Verdict{Known: true, Detail: "no backup unit is active (checked: " + strings.Join(idle, ", ") + ")"}
}

func defaultRunner(ctx context.Context, name string, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// The output is captured and discarded: only the exit code is meaningful,
	// and a status script that prints a megabyte must not fill a pipe.
	cmd.Stdout = nil
	cmd.Stderr = nil
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), err
	}
	return -1, err
}
