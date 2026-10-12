package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
)

// This check is what stands between a power cut and a scan that decides every
// photo is gone. After the array fails to assemble, /mnt/raid0/photos is just an
// empty directory on the system SSD: a scan that ran anyway would mark all 5684
// rows missing, and a thumbnail run would write a cache for the wrong tree.
//
// Every refusal below is a case where running would be worse than not running.

func collection(code, root string, opts ...func(*config.Collection)) config.Collection {
	c := config.Collection{Code: code, Name: code, Root: root, Enabled: true}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func TestAVanishedRootRefusesToRun(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted")
	got := Verify(collection("photos", missing), t.TempDir(), "")

	if got.OK {
		t.Fatal("a collection whose root does not exist must not be considered safe to run")
	}
	if got.Fatal == "" {
		t.Fatal("a missing root must be reported as a refusal, not as a silent note")
	}
	if !strings.Contains(got.Fatal, "not accessible") {
		t.Errorf("Fatal = %q, want it to say the root is not accessible", got.Fatal)
	}
	if !strings.Contains(got.Describe(), "REFUSED") {
		t.Error("Describe must render a refusal as REFUSED so an operator cannot miss it")
	}
}

func TestARootThatIsNotADirectoryRefusesToRun(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "photos")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Verify(collection("photos", file), t.TempDir(), "")
	if got.OK {
		t.Fatal("a root that is a regular file cannot be walked and must be refused")
	}
	if !strings.Contains(got.Fatal, "not a directory") {
		t.Errorf("Fatal = %q, want it to name the real problem", got.Fatal)
	}
}

func TestTheSystemFilesystemIsRefusedWhenTheDataVolumeIsNotMounted(t *testing.T) {
	// The scenario: the mount did not come back, so the configured path now
	// resolves onto the SSD that holds the system and the index.
	root := t.TempDir()
	state := t.TempDir()

	got := Verify(collection("photos", root), state, "")
	if got.OK {
		t.Skip("this host keeps the media and state trees on different filesystems, so the " +
			"fallback-onto-the-system-disk case cannot be reproduced here")
	}
	if !strings.Contains(got.Fatal, "system filesystem") &&
		!strings.Contains(got.Fatal, "same filesystem as the albumctl state") {
		t.Errorf("Fatal = %q, want it to explain that the expected volume is not mounted", got.Fatal)
	}
}

func TestASharedVolumeIsAllowedOnlyWhenTheOperatorSaysSo(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()

	// With the escape hatch closed, a tree on the state's own filesystem is
	// refused. With it open, the check is skipped *and* a note is recorded, so
	// the relaxation is never silent.
	strict := Verify(collection("photos", root), state, "")
	relaxed := Verify(collection("photos", root, func(c *config.Collection) {
		c.AllowSharedVolume = true
	}), state, "")

	if strict.OK && !relaxed.OK {
		t.Fatal("the relaxed case cannot be refused while the strict one is accepted")
	}
	if !relaxed.OK {
		t.Fatalf("allow_shared_volume must let this run, got Fatal = %q", relaxed.Fatal)
	}
	notes := strings.Join(relaxed.Notes, "\n")
	if !strings.Contains(notes, "allow_shared_volume") {
		t.Error("relaxing the volume check must leave a note saying that a missing mount can no " +
			"longer be detected this way")
	}
	if len(strict.Notes) > 0 && strings.Contains(strings.Join(strict.Notes, "\n"), "allow_shared_volume") {
		t.Error("the strict check must not report a relaxation it did not make")
	}
}

func TestAChangedFilesystemIdentityRefusesToRun(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()

	real, err := fsutil.FSID(root)
	if err != nil || real == "" {
		t.Skipf("this host does not expose a filesystem id for %s (%v), so the pin cannot be "+
			"exercised here", root, err)
	}

	// A pin that matches passes.
	same := Verify(collection("photos", root, func(c *config.Collection) {
		c.AllowSharedVolume = true
	}), state, real)
	if !same.OK {
		t.Fatalf("a matching pin must not refuse, got Fatal = %q", same.Fatal)
	}

	// A pin that differs means the path now points at different storage: the
	// index describes one volume and the files come from another.
	other := "00000000-0000-0000-0000-000000000000"
	if real == other {
		other = "11111111-1111-1111-1111-111111111111"
	}
	changed := Verify(collection("photos", root, func(c *config.Collection) {
		c.AllowSharedVolume = true
	}), state, other)
	if changed.OK {
		t.Fatal("a changed filesystem id must refuse to run: the index and the files would " +
			"describe different volumes")
	}
	if !strings.Contains(changed.Fatal, "filesystem id changed") {
		t.Errorf("Fatal = %q, want it to name the changed identity", changed.Fatal)
	}
	// The operator needs to be told how to recover deliberately.
	if !strings.Contains(changed.Fatal, "reset-fsid") {
		t.Error("the refusal must say how to accept a deliberate replacement")
	}
}

func TestARootOutsideItsDataMountRefusesToRun(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()

	got := Verify(collection("photos", root, func(c *config.Collection) {
		c.DataMount = elsewhere
		c.AllowSharedVolume = true
	}), t.TempDir(), "")
	if got.OK {
		t.Fatal("a root that is not inside its configured data_mount must be refused")
	}
	if !strings.Contains(got.Fatal, "data_mount") {
		t.Errorf("Fatal = %q, want it to name data_mount", got.Fatal)
	}
}

func TestADataMountThatIsNotMountedRefusesToRun(t *testing.T) {
	// This is the exact condition after a failed array assembly: the configured
	// data_mount path exists, but nothing is mounted on it.
	base := t.TempDir()
	root := filepath.Join(base, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	mp, err := fsutil.IsMountPoint(base)
	if err != nil {
		t.Skipf("cannot determine mount points on this host: %v", err)
	}
	if mp {
		t.Skipf("%s is a mount point on this host, so the unmounted case cannot be built here", base)
	}

	got := Verify(collection("photos", root, func(c *config.Collection) {
		c.DataMount = base
		c.AllowSharedVolume = true
	}), t.TempDir(), "")
	if got.OK {
		t.Fatal("a data_mount that is not a mount point must refuse to run, so that a missing " +
			"mount cannot look like an empty library")
	}
	if !strings.Contains(got.Fatal, "not a mount point") {
		t.Errorf("Fatal = %q, want it to say the volume is not mounted", got.Fatal)
	}
	if got.IsMount {
		t.Error("IsMount must not be reported true for a path that is not a mount point")
	}
}

func TestARealMountPointIsAccepted(t *testing.T) {
	// The positive case needs a real mount point, and "/" is the one that always
	// exists. The media tree stays where it is; only the data_mount claim is
	// real, which is what makes this a test of the check rather than of the host.
	root := t.TempDir()
	mp, err := fsutil.IsMountPoint("/")
	if err != nil {
		t.Skipf("cannot determine mount points here: %v", err)
	}
	if !mp {
		t.Skip("this host does not report / as a mount point")
	}

	got := Verify(collection("photos", root, func(c *config.Collection) {
		c.DataMount = "/"
		c.AllowSharedVolume = true
	}), t.TempDir(), "")
	if !got.OK {
		t.Fatalf("a real mount point must be accepted, got Fatal = %q", got.Fatal)
	}
	if !got.IsMount {
		t.Error("IsMount must be true when data_mount really is a mount point")
	}
	if got.RootDevice == 0 {
		t.Error("the root device must be recorded: it is the identity that makes the check " +
			"independent of the path string")
	}
}

func TestASymlinkedRootIsResolvedAndNoted(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-photos")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "photos")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	got := Verify(collection("photos", link, func(c *config.Collection) {
		c.AllowSharedVolume = true
	}), t.TempDir(), "")
	if !got.OK {
		t.Fatalf("a symlinked root that resolves to an existing directory is usable, got %q", got.Fatal)
	}
	if !strings.Contains(strings.Join(got.Notes, "\n"), "real path") {
		t.Error("the resolved path must be noted: an operator needs to know which tree is being read")
	}
}

func TestDescribeReportsTheIdentityAndEveryNote(t *testing.T) {
	root := t.TempDir()
	got := Verify(collection("photos", root, func(c *config.Collection) {
		c.AllowSharedVolume = true
		c.DataMount = "/"
	}), t.TempDir(), "")

	d := got.Describe()
	if !strings.Contains(d, "root="+root) {
		t.Errorf("Describe did not report the root:\n%s", d)
	}
	if !strings.Contains(d, "fsid=") {
		t.Errorf("Describe did not report the filesystem id:\n%s", d)
	}
	if !strings.Contains(d, "data_mount=/ is_mount_point=true") {
		t.Errorf("Describe did not report the mount check:\n%s", d)
	}
	if strings.Contains(d, "REFUSED") {
		t.Errorf("Describe rendered a refusal for a check that passed:\n%s", d)
	}
}
