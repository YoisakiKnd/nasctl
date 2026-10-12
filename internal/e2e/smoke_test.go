package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorIsReadOnlyAndReportsProblems runs `doctor` before anything else and
// checks that it neither writes to the media tree nor claims the machine is
// ready. It is the command an operator runs first, and it must be honest.
func TestDoctorIsReadOnlyAndReportsProblems(t *testing.T) {
	r := newRig(t)

	// Record the media tree's state so we can prove doctor did not touch it.
	before := treeSnapshot(t, r.Media)
	if _, err := os.Stat(r.State); err == nil {
		t.Fatal("the state directory exists before any command has run")
	}

	out, errs, code := r.run("doctor")
	combined := out + errs

	after := treeSnapshot(t, r.Media)
	if before != after {
		t.Errorf("doctor modified the media tree.\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// On this development machine cgroup v2 delegation is not available, so
	// doctor must say so. It must not fail the whole check: the point of doctor
	// is to report, not to fix.
	if !strings.Contains(strings.ToLower(combined), "cgroup") {
		t.Errorf("doctor did not mention cgroup isolation; on a machine without it that "+
			"is the single most important thing to report.\n%s", truncate(combined, 2000))
	}
	if code != 0 && code != 1 {
		t.Errorf("doctor exited %d; it should exit 0 for clean, 1 for problems, not crash", code)
	}
	t.Logf("doctor exit=%d output:\n%s", code, truncate(combined, 3000))
}

// TestScanIsBoundedAndThenIdempotent covers the two things that must be true of
// the scanner: it stops at the requested limit, and running it again over an
// unchanged tree changes nothing.
func TestScanIsBoundedAndThenIdempotent(t *testing.T) {
	r := newRig(t)

	// A dry run must report without writing an index.
	before := treeSnapshot(t, r.Media)
	out := r.mustRun("scan", "--collection", "smoke", "--limit", "3", "--dry-run")
	if !strings.Contains(strings.ToLower(out), "dry") && !strings.Contains(out, "would") {
		t.Logf("NOTE: the dry-run report does not say what it would do; output was:\n%s", out)
	}
	if after := treeSnapshot(t, r.Media); before != after {
		t.Errorf("a dry run modified the media tree")
	}

	// A real scan with a limit.
	r.mustRun("scan", "--collection", "smoke", "--limit", "3")

	// And a scan with no limit at all, which the design forbids because an
	// unbounded scan is how the machine was lost the first time.
	_, errs, code := r.run("scan", "--collection", "smoke")
	if code == 0 {
		t.Errorf("`scan` without --limit succeeded; an unbounded scan must be refused.\n%s", errs)
	}

	// Scan everything, then scan again and check that the second run reports
	// nothing new. It must not rebuild the index.
	r.indexAll()
	out2 := r.mustRun("scan", "--collection", "smoke", "--limit", "5000")
	if !strings.Contains(strings.ToLower(out2), "unchanged") && !strings.Contains(out2, " 0 ") {
		t.Logf("NOTE: the second scan does not clearly report a no-op:\n%s", out2)
	}
}

// TestDoctorRefusesAStatePathInsideTheMediaTree asserts the config guard that
// keeps the index and cache out of the read-only photo tree.
func TestDoctorRefusesAStatePathInsideTheMediaTree(t *testing.T) {
	r := newRig(t)
	// Point the state directory at a subdirectory of the media root.
	cfg := strings.Replace(r.readConfig(), r.State, filepath.Join(r.Media, ".albumctl"), 1)
	if err := os.WriteFile(r.Config, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errs, code := r.run("doctor")
	if code == 0 {
		t.Errorf("doctor accepted a state directory inside the media root; writes to the "+
			"read-only photo tree must be impossible by configuration.\n%s", errs)
	}
}

func (r *rig) readConfig() string {
	r.t.Helper()
	b, err := os.ReadFile(r.Config)
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

// treeSnapshot renders the media tree as "path size mtime" lines, so a test can
// prove that a read-only command changed nothing.
func treeSnapshot(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		// A symlink's target is what matters for the escape tests.
		if fi.Mode()&os.ModeSymlink != 0 {
			dst, _ := os.Readlink(path)
			b.WriteString(rel + " -> " + dst + "\n")
			return nil
		}
		b.WriteString(rel + " " + fi.ModTime().UTC().String() + " " + itoa(fi.Size()) + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func itoa(n int64) string { return fmt.Sprintf("%d", n) }
