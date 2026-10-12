package resource

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests exercise the stop path with real processes. The guarantee being
// checked is the one that keeps a killed decode from leaving an orphan behind on
// a 981 MiB machine: the *whole* process group goes away, and the supervisor can
// tell truthfully whether it did.
//
// Everything here spawns its own children in their own process group and reaps
// them, so the test binary never leaves a stray process behind itself.

func spawnInOwnGroup(t *testing.T, script string) (*exec.Cmd, int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start %q: %v", script, err)
	}
	pgid := cmd.Process.Pid // Setpgid puts the child in a group named after its pid
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	})
	return cmd, pgid
}

// writeFakeStat writes a syntactically valid /proc/<pid>/stat.
//
// The field order after the comm field is fixed by procfs: state, ppid, pgrp,
// session, tty_nr, tpgid, flags, minflt, cminflt, majflt, cmajflt, utime,
// stime, cutime, cstime, priority, nice, num_threads, itrealvalue, starttime.
// A short line is not a parse error the reader forgives: it means the columns
// after it cannot be trusted.
func writeFakeStat(t *testing.T, root string, pid int, comm string, state byte, ppid, pgid, threads int, startTicks uint64) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fields := []string{
		string(state),      // 0  state
		strconv.Itoa(ppid), // 1  ppid
		strconv.Itoa(pgid), // 2  pgrp
		"0", "0", "0", "0", // 3-6  session, tty_nr, tpgid, flags
		"0", "0", "0", "0", // 7-10 minflt, cminflt, majflt, cmajflt
		"0", "0", "0", "0", // 11-14 utime, stime, cutime, cstime
		"20", "0", // 15-16 priority, nice
		strconv.Itoa(threads),              // 17 num_threads
		"0",                                // 18 itrealvalue
		strconv.FormatUint(startTicks, 10), // 19 starttime
	}
	line := strconv.Itoa(pid) + " (" + comm + ") " + strings.Join(fields, " ") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitForState polls until /proc/<pid>/stat reports the wanted state.
func waitForState(t *testing.T, pid int, want byte, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := ReadProcStat("/proc", pid); err == nil && st.State == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestTerminateGroupKillsChildrenToo(t *testing.T) {
	// The shell forks two sleeps and waits. They are in the same group, so
	// signalling the group - not the process - is what removes them.
	_, pgid := spawnInOwnGroup(t, "sleep 30 & sleep 30 & wait")

	if !ProcessGroupAlive(pgid) {
		t.Fatal("the group should be alive before we stop it")
	}

	terminated, escalated, err := TerminateGroup(pgid, 3*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateGroup: %v", err)
	}
	if !terminated {
		t.Error("TerminateGroup reported the group still running after the grace period")
	}
	if ProcessGroupAlive(pgid) {
		t.Error("a process remains in the group: a killed decode must not leave children behind")
	}
	t.Logf("terminated=%v escalated=%v", terminated, escalated)
}

func TestTerminateGroupEscalatesForAWorkerThatIgnoresSIGTERM(t *testing.T) {
	// A decoder that traps SIGTERM must still be stopped: the grace period is a
	// courtesy, not a negotiation.
	_, pgid := spawnInOwnGroup(t, `trap "" TERM; sleep 30`)

	if !ProcessGroupAlive(pgid) {
		t.Fatal("the group should be alive before we stop it")
	}
	terminated, escalated, err := TerminateGroup(pgid, 300*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateGroup: %v", err)
	}
	if !terminated {
		t.Fatal("a group that ignores SIGTERM must still be gone after SIGKILL")
	}
	if !escalated {
		t.Error("escalated = false, but the worker ignored SIGTERM and had to be SIGKILLed")
	}
	if ProcessGroupAlive(pgid) {
		t.Error("the group is still alive after escalation")
	}
}

func TestTerminateGroupRefusesToSignalAGroupItMustNot(t *testing.T) {
	for _, pgid := range []int{0, 1, -5} {
		terminated, escalated, err := TerminateGroup(pgid, 50*time.Millisecond, 10*time.Millisecond)
		if err == nil {
			t.Errorf("TerminateGroup(%d) returned no error; it must refuse rather than risk "+
				"signalling init or another service's group", pgid)
		}
		if terminated || escalated {
			t.Errorf("TerminateGroup(%d) = terminated=%v escalated=%v, want false/false on refusal",
				pgid, terminated, escalated)
		}
	}
}

func TestTerminateGroupOnAnEmptyGroupReturnsImmediately(t *testing.T) {
	cmd, pgid := spawnInOwnGroup(t, "exit 0")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for the short-lived child: %v", err)
	}

	start := time.Now()
	terminated, escalated, err := TerminateGroup(pgid, 2*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("TerminateGroup on an empty group: %v", err)
	}
	if !terminated || escalated {
		t.Errorf("terminated=%v escalated=%v, want true/false for a group with nothing in it",
			terminated, escalated)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("an empty group took %s to report; it should not wait out the grace period", elapsed)
	}
}

// A process that has exited and not yet been reaped is still in the process
// table, so signal 0 still succeeds for its group. Reporting that as "alive"
// makes the supervisor claim a well-behaved worker "ignored SIGTERM and was
// SIGKILLed after the grace period" - a false statement about a worker that
// exited promptly, and one that hides the real case.
func TestAReapedGroupIsNotAlive(t *testing.T) {
	cmd, pgid := spawnInOwnGroup(t, "exit 0")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for the short-lived child: %v", err)
	}
	if ProcessGroupAlive(pgid) {
		t.Error("ProcessGroupAlive says an empty group is alive")
	}
}

func TestTheGroupScanDistinguishesZombiesFromRunningProcesses(t *testing.T) {
	// A synthetic /proc keeps this deterministic: no timing, no signals.
	root := t.TempDir()
	writeStat := func(pid, pgid int, state byte) {
		writeFakeStat(t, root, pid, "sh", state, 1, pgid, 3, 12345)
	}

	// A zombie in the group and a running process in another group.
	writeStat(100, 4200, 'Z')
	writeStat(101, 4300, 'R')
	writeStat(102, 4200, 'S')

	live, err := groupHasLiveProcess(root, 4200)
	if err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Error("group 4200 holds a running process; the scan said it was empty")
	}

	// The only process left in the group has exited.
	if err := os.RemoveAll(filepath.Join(root, "102")); err != nil {
		t.Fatal(err)
	}
	live, err = groupHasLiveProcess(root, 4200)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		t.Error("a group holding only a zombie was reported as alive; the supervisor would " +
			"then claim the worker ignored SIGTERM")
	}

	// An unrelated group is not confused for ours.
	live, err = groupHasLiveProcess(root, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if live {
		t.Error("group 9999 does not exist")
	}

	// Unsigned "pid 0" entries in /proc are skipped by ReadDir's numeric filter,
	// so they cannot be mistaken for a member of any group.
	if err := os.MkdirAll(filepath.Join(root, "0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if live, err := groupHasLiveProcess(root, 0); err != nil || live {
		t.Errorf("group 0: live=%v err=%v, want false/nil", live, err)
	}
}

func TestProcessGroupAliveRejectsAGroupThisProgramMustNotName(t *testing.T) {
	for _, pgid := range []int{0, -1} {
		if ProcessGroupAlive(pgid) {
			t.Errorf("ProcessGroupAlive(%d) = true; group 0 is the caller's own group and is "+
				"never a worker", pgid)
		}
	}
}

func TestReadProcStatSurvivesACommWithSpacesAndParens(t *testing.T) {
	// The comm field is parenthesised and may contain both ")" and spaces: a
	// naive split would read the wrong columns and mis-attribute a process to a
	// group, which is a stop path that kills the wrong thing.
	root := t.TempDir()
	pid := 4242
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeStat(t, root, pid, "my ) weird (worker", 'S', 1, 4200, 7, 999)
	st, err := ReadProcStat(root, pid)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != 'S' {
		t.Errorf("State = %q, want 'S': the parse must anchor on the last ')'", string(st.State))
	}
	if st.PGID != 4200 {
		t.Errorf("PGID = %d, want 4200", st.PGID)
	}
	if st.PPID != 1 {
		t.Errorf("PPID = %d, want 1", st.PPID)
	}
	if st.NumThreads != 7 {
		t.Errorf("NumThreads = %d, want 7", st.NumThreads)
	}
	if st.StartTicks != 999 {
		t.Errorf("StartTicks = %d, want 999", st.StartTicks)
	}
}

func TestProcessGroupAliveIsFalseOnceARealGroupHasExited(t *testing.T) {
	// The end-to-end form of the same question, with a real process: the group
	// must read as empty after it exits, including while it is an unreaped
	// zombie.
	cmd, pgid := spawnInOwnGroup(t, "exit 0")
	if !waitForState(t, cmd.Process.Pid, 'Z', 2*time.Second) {
		t.Fatalf("pid %d never became a zombie; the test rig is not doing what it says",
			cmd.Process.Pid)
	}
	if ProcessGroupAlive(pgid) {
		t.Error("a group whose only member has exited is reported as alive (its process table " +
			"entry is a zombie, not a running worker)")
	}
}
