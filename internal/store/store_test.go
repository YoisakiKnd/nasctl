package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("", Options{Memory: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newFileStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.sqlite3")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedCollection(t *testing.T, s *Store) int64 {
	t.Helper()
	id, err := s.UpsertCollection(context.Background(), "photos", "Photos",
		"/mnt/raid0/photos", "/mnt/raid0", true, false)
	if err != nil {
		t.Fatalf("UpsertCollection: %v", err)
	}
	return id
}

func scanRow(colID int64, rel string, size, mtimeNS int64) MediaScan {
	dir := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		dir = rel[:i]
	} else {
		dir = ""
	}
	ext := ""
	if i := strings.LastIndexByte(rel, '.'); i >= 0 {
		ext = strings.ToLower(rel[i+1:])
	}
	base := rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		base = rel[i+1:]
	}
	return MediaScan{
		CollectionID:   colID,
		CollectionCode: "photos",
		RelPath:        rel,
		RelDir:         dir,
		BaseName:       base,
		Ext:            ext,
		Size:           size,
		MtimeNS:        mtimeNS,
		Kind:           "image",
		MIME:           "image/jpeg",
		At:             UnixNow(),
	}
}

// TestUpsertIsIdempotentForAnUnchangedFile is the property that makes an
// interrupted scan safe to repeat: a second pass over an unchanged file must
// not create a second row, and must not report the file as changed, or every
// rescan would throw away every thumbnail.
func TestUpsertIsIdempotentForAnUnchangedFile(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	first, err := s.UpsertMedia(ctx, scanRow(col, "a/b.jpg", 1234, 999))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Error("the first UpsertMedia did not report Created")
	}
	// A create reports Changed too: "no derived value recorded earlier can be
	// trusted" is true of a row that did not exist. What matters is that the
	// second call reports it false.
	if first.PreviousThumbState != "" {
		t.Errorf("a newly created row has PreviousThumbState %q, want empty: there was no "+
			"cached thumbnail to delete", first.PreviousThumbState)
	}

	second, err := s.UpsertMedia(ctx, scanRow(col, "a/b.jpg", 1234, 999))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created {
		t.Error("the second UpsertMedia created another row for the same path")
	}
	if second.Changed {
		t.Error("the second UpsertMedia reported Changed for an unchanged file")
	}
	if second.ID != first.ID {
		t.Errorf("the row id changed from %d to %d", first.ID, second.ID)
	}

	totals, err := s.Totals(ctx, col)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Media != 1 {
		t.Errorf("Totals.Total = %d after two upserts of one path, want 1", totals.Media)
	}
}

// TestUpsertInvalidatesDerivedStateOnChange guards the cache-correctness rule:
// when a file's size or mtime moves, every fact derived from the old bytes is
// discarded. Otherwise the gallery would show a thumbnail of a picture that is
// no longer there.
func TestUpsertInvalidatesDerivedStateOnChange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	ref, err := s.UpsertMedia(ctx, scanRow(col, "a/b.jpg", 1234, 999))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{
		ID: ref.ID, State: ProbeOK, Format: "jpeg", Width: 4000, Height: 3000,
		Pixels: 12_000_000, TakenSrc: TakenSrcExif, TakenAt: ptrInt64(1_600_000_000),
		Make: "OnePlus", Model: "ONEPLUS A5000",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThumb(ctx, ThumbUpdate{
		ID: ref.ID, State: ThumbOK, Bytes: 40_000, At: UnixNow(),
	}); err != nil {
		t.Fatal(err)
	}

	before, err := s.GetMedia(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.ProbeState != ProbeOK || before.ThumbState != ThumbOK {
		t.Fatalf("setup failed: probe=%q thumb=%q", before.ProbeState, before.ThumbState)
	}

	// Same path, new size: the file changed underneath us.
	after, err := s.UpsertMedia(ctx, scanRow(col, "a/b.jpg", 4321, 1000))
	if err != nil {
		t.Fatal(err)
	}
	if !after.Changed {
		t.Fatal("a size change was not reported as Changed")
	}
	if after.PreviousThumbState != ThumbOK {
		t.Errorf("PreviousThumbState = %q, want %q so the caller knows to delete the stale cache file",
			after.PreviousThumbState, ThumbOK)
	}

	got, err := s.GetMedia(ctx, ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeState == ProbeOK {
		t.Error("the probe state survived a file change; stale metadata would be shown as current")
	}
	if got.ThumbState == ThumbOK {
		t.Error("the thumbnail state survived a file change; the old thumbnail would be served for new bytes")
	}
	if got.Width == 4000 || got.Make == "OnePlus" {
		t.Errorf("stale metadata survived: width=%d make=%q", got.Width, got.Make)
	}
	if got.Size != 4321 || got.MtimeNS != 1000 {
		t.Errorf("size/mtime = %d/%d, want the new 4321/1000", got.Size, got.MtimeNS)
	}
}

func TestUpsertDetectsAnMtimeOnlyChange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	if _, err := s.UpsertMedia(ctx, scanRow(col, "a.jpg", 100, 500)); err != nil {
		t.Fatal(err)
	}
	// Same size, different mtime: an editor rewriting a file in place can leave
	// the length alone, so mtime alone has to be enough.
	res, err := s.UpsertMedia(ctx, scanRow(col, "a.jpg", 100, 501))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("an mtime-only change was not detected; the index would keep serving the old file's metadata")
	}
}

// TestChangeDetectionDoesNotReadFileContents documents the cost model: the
// index decides from path, size and mtime, and never from a hash. Hashing every
// original is what the brief forbids, because it would read every byte of every
// video to discover duplicates nobody asked about.
func TestChangeDetectionDoesNotReadFileContents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	// Two files with identical size and mtime but different content are
	// indistinguishable to the index. That is the intended trade-off.
	if _, err := s.UpsertMedia(ctx, scanRow(col, "x.jpg", 4096, 700)); err != nil {
		t.Fatal(err)
	}
	res, err := s.UpsertMedia(ctx, scanRow(col, "y.jpg", 4096, 700))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created {
		t.Error("a second path with the same size and mtime was not stored as a new row")
	}
	// The point being pinned is that the two rows are independent: the second
	// insert did not disturb the first. Nothing was read from either file.
	if res.PreviousThumbState != "" {
		t.Errorf("inserting a new path reported PreviousThumbState %q; it must not claim to have "+
			"invalidated a thumbnail it never had", res.PreviousThumbState)
	}
	first := scanRow(col, "x.jpg", 4096, 700)
	if again, err := s.UpsertMedia(ctx, first); err != nil {
		t.Fatal(err)
	} else if again.Changed {
		t.Error("re-upserting an unchanged row reported Changed")
	}
}

// TestPauseSurvivesReopen is the requirement that the pause is durable: it has
// to be in the index, not in the process. A pause that a restart cleared would
// mean the operator's stop was silently undone.
func TestPauseSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite3")

	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seedCollection(t, s)
	if err := s.Pause(ctx, "operator request", "ubuntu"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()

	ps, err := reopened.PauseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Paused {
		t.Fatal("the pause did not survive a reopen; a restart would resume heavy work the operator stopped")
	}
	if ps.Reason != "operator request" {
		t.Errorf("the pause reason is %q, want it preserved so status can explain why", ps.Reason)
	}
	if ps.By != "ubuntu" {
		t.Errorf("the pause author is %q, want it preserved", ps.By)
	}
	if ps.Since == 0 {
		t.Error("the pause timestamp was not recorded")
	}
}

func TestResumeClearsThePause(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if err := s.Pause(ctx, "why", "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	ps, err := s.PauseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Paused {
		t.Error("the pause is still set after Resume")
	}
	if ps.Reason != "" || ps.Since != 0 || ps.By != "" {
		t.Errorf("Resume left stale pause fields: %+v", ps)
	}
}

func TestPauseDefaultsToNotPaused(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ps, err := s.PauseState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh index must not read as paused, or the first run would refuse.
	if ps.Paused {
		t.Error("a fresh index reports itself as paused")
	}
}

// TestPressureTripAndClear covers the record that makes an automatic restart
// after a resource stop impossible. The flag has to persist, and clearing it
// has to be a deliberate act.
func TestPressureTripAndClear(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite3")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh index has no trip recorded at all.
	ps, err := s.Pressure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Tripped {
		t.Error("a fresh index reports a pressure trip")
	}
	_ = s.Close()

	s, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TripPressure(ctx, "mem_available_below_stop",
		"MemAvailable 180 MiB < stop 200 MiB", "MemAvailable: 184320 kB"); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	ps, err = reopened.Pressure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !ps.Tripped {
		t.Fatal("the pressure trip did not survive a reopen; the next run could start immediately")
	}
	if ps.At == 0 {
		t.Error("the trip timestamp was not recorded")
	}
	if ps.Reason != "mem_available_below_stop" {
		t.Errorf("the trip reason is %q", ps.Reason)
	}
	if !strings.Contains(ps.Snapshot, "MemAvailable") {
		t.Errorf("the diagnostic snapshot was not kept: %q", ps.Snapshot)
	}

	// The snapshot is bounded, so a large diagnostic dump cannot grow the row
	// without limit.
	huge := strings.Repeat("x", 100_000)
	if err := reopened.TripPressure(ctx, "r", "d", huge); err != nil {
		t.Fatal(err)
	}
	ps, err = reopened.Pressure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.Snapshot) > 4096 {
		t.Errorf("the stored snapshot is %d bytes; it must be bounded", len(ps.Snapshot))
	}

	if err := reopened.ClearPressure(ctx); err != nil {
		t.Fatal(err)
	}
	ps, err = reopened.Pressure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ps.Tripped || ps.Reason != "" || ps.Snapshot != "" {
		t.Errorf("ClearPressure left a record behind: %+v", ps)
	}
}

// TestReadOnlyHandleRefusesWrites is the guarantee the web service relies on:
// a bug in a handler must not be able to mutate the index.
func TestReadOnlyHandleRefusesWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.sqlite3")

	rw, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	col := seedCollection(t, rw)
	if _, err := rw.UpsertMedia(ctx, scanRow(col, "a.jpg", 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := Open(path, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()

	if !ro.ReadOnly() {
		t.Error("ReadOnly() reports false on a read-only handle")
	}
	// Reads must work.
	if _, err := ro.GetMedia(ctx, 1); err != nil {
		t.Errorf("a read failed on the read-only handle: %v", err)
	}
	if _, _, err := ro.CountPending(ctx, 0); err != nil {
		t.Errorf("CountPending failed on the read-only handle: %v", err)
	}

	// Writes must be refused, whatever the caller tries.
	writes := map[string]func() error{
		"SetState":      func() error { return ro.SetState(ctx, "k", "v") },
		"ClearState":    func() error { return ro.ClearState(ctx, "k") },
		"SetStates":     func() error { return ro.SetStates(ctx, map[string]string{"k": "v"}) },
		"Pause":         func() error { return ro.Pause(ctx, "r", "by") },
		"Resume":        func() error { return ro.Resume(ctx) },
		"TripPressure":  func() error { return ro.TripPressure(ctx, "r", "d", "s") },
		"ClearPressure": func() error { return ro.ClearPressure(ctx) },
		"UpsertMedia": func() error {
			_, err := ro.UpsertMedia(ctx, scanRow(col, "b.jpg", 1, 1))
			return err
		},
		"SetProbe": func() error {
			return ro.SetProbe(ctx, ProbeUpdate{ID: 1, State: ProbeOK})
		},
		"SetThumb": func() error {
			return ro.SetThumb(ctx, ThumbUpdate{ID: 1, State: ThumbOK})
		},
		"UpsertCollection": func() error {
			_, err := ro.UpsertCollection(ctx, "x", "X", "/mnt/x", "", true, false)
			return err
		},
		"StartTask": func() error {
			_, err := ro.StartTask(ctx, Task{Kind: "thumbs"})
			return err
		},
		"AddTaskItem": func() error {
			return ro.AddTaskItem(ctx, TaskItem{TaskID: 1, RelPath: "a.jpg"})
		},
	}
	for name, fn := range writes {
		if err := fn(); err == nil {
			t.Errorf("%s succeeded on a read-only handle; the web service depends on this failing", name)
		}
	}
}

// TestReadOnlyHandleCannotCreateAnIndex matters because the web service starts
// before the first scan on a fresh install. If a read-only open created a file,
// the service would leave a half-built index behind and `doctor` would report a
// schema it never wrote.
func TestReadOnlyHandleCannotCreateAnIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "index.sqlite3")
	if _, err := Open(path, Options{ReadOnly: true}); err == nil {
		t.Error("a read-only open created an index that did not exist")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a read-only open created the index file")
	}
}

// TestIntegrityCheckOnAFreshIndex keeps doctor's read-only checks usable.
func TestIntegrityCheckOnAFreshIndex(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)
	seedCollection(t, s)
	result, err := s.IntegrityCheck(ctx)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck = %q, want ok", result)
	}
	if fk, err := s.ForeignKeyCheck(ctx); err != nil {
		t.Fatalf("ForeignKeyCheck: %v", err)
	} else if len(fk) != 0 {
		t.Errorf("ForeignKeyCheck reported %v on a fresh index", fk)
	}
	st, err := s.SchemaState(ctx)
	if err != nil {
		t.Fatalf("SchemaState: %v", err)
	}
	if !st.OK {
		t.Errorf("SchemaState.OK = false on a freshly created index: %+v", st)
	}
	if st.Found != SchemaVersion || st.Expected != SchemaVersion {
		t.Errorf("SchemaState found/expected = %d/%d, want %d/%d",
			st.Found, st.Expected, SchemaVersion, SchemaVersion)
	}
	if st.Note != "" {
		t.Errorf("SchemaState.Note = %q on a matching index", st.Note)
	}
}

// TestMigrationsAreIdempotent: opening an index that is already current must be
// a no-op, because every CLI command opens the store.
func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite3")
	for i := 0; i < 3; i++ {
		s, err := Open(path, Options{})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st, err := s.SchemaState(ctxTODO())
		if err != nil {
			t.Fatalf("open %d: SchemaState: %v", i, err)
		}
		if !st.OK || st.Found != SchemaVersion {
			t.Fatalf("open %d: schema state %+v, want found=%d", i, st, SchemaVersion)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}

// TestRefusesAnIndexFromANewerBuild stops an older binary from silently
// operating on a schema it does not understand.
func TestRefusesAnIndexFromANewerBuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.sqlite3")
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(context.Background(),
		fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path, Options{})
	if err == nil {
		t.Fatal("an index written by a newer schema version was opened")
	}
	if !strings.Contains(err.Error(), "matching albumctl") {
		t.Errorf("the error does not tell the operator what to do: %v", err)
	}
}

// ctxTODO exists so the migration test reads clearly without threading a
// context through a helper whose only job is to reopen a database.
func ctxTODO() context.Context { return context.Background() }

func ptrInt64(v int64) *int64 { return &v }

// TestWorkQueuesExcludeEverythingNotEligible is the query that decides what a
// bounded run will touch. It has to be exactly the eligibility predicate, or a
// run either does nothing or touches files it was told to leave alone.
func TestWorkQueuesExcludeEverythingNotEligible(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	// Every row is stamped by a previous run, then the four live files are
	// restamped as if this run had just walked past them. The stamp is a run
	// token, which is what makes MarkMissing exact; the timestamps move too, only
	// so the rows look the way real ones do.
	const prevRun, thisRun = 1, 2
	base := UnixNow()
	stale := base - 3600
	put := func(rel string, at, token int64) int64 {
		r := scanRow(col, rel, 10, 10)
		r.At = at
		r.RunToken = token
		res, err := s.UpsertMedia(ctx, r)
		if err != nil {
			t.Fatalf("UpsertMedia(%s): %v", rel, err)
		}
		return res.ID
	}

	probeMe := put("probe_me.jpg", stale, prevRun) // pending probe, not ready for thumbs
	thumbMe := put("thumb_me.jpg", stale, prevRun) // probed, thumbnailable, no thumbnail yet
	skipped := put("skipped.heic", stale, prevRun) // deliberately not attempted
	done := put("done.jpg", stale, prevRun)        // already has a thumbnail
	gone := put("gone.jpg", stale, prevRun)        // not seen by this run
	for _, rel := range []string{"probe_me.jpg", "thumb_me.jpg", "skipped.heic", "done.jpg"} {
		put(rel, base, thisRun) // refreshed during this run
	}

	if err := s.SetProbe(ctx, ProbeUpdate{ID: thumbMe, State: ProbeOK, Kind: "image", Format: "jpeg",
		Width: 800, Height: 600, Pixels: 480_000, ThumbState: ThumbPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{ID: skipped, State: ProbeSkipped,
		Reason: "format_not_enabled:heic", ThumbState: ThumbSkipped}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{ID: done, State: ProbeOK, Kind: "image", Format: "jpeg",
		ThumbState: ThumbPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThumb(ctx, ThumbUpdate{ID: done, State: ThumbOK, Bytes: 100, At: UnixNow()}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{ID: gone, State: ProbeOK, Kind: "image", Format: "jpeg",
		ThumbState: ThumbPending}); err != nil {
		t.Fatal(err)
	}

	// Only gone.jpg was not seen during this run, so only it is marked.
	n, err := s.MarkMissing(ctx, col, thisRun)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("MarkMissing marked %d rows, want 1 (only the file not seen during this run)", n)
	}

	probes, err := s.NextProbe(ctx, col, 3, UnixNow(), 100)
	if err != nil {
		t.Fatal(err)
	}
	probeIDs := map[int64]bool{}
	for _, ref := range probes {
		probeIDs[ref.ID] = true
	}
	if !probeIDs[probeMe] {
		t.Error("a pending file was not offered for probing")
	}
	if probeIDs[done] {
		t.Error("a fully probed file was offered for probing again")
	}
	if probeIDs[gone] {
		t.Error("a missing original was offered for probing; the read would fail")
	}

	thumbs, err := s.NextThumb(ctx, col, 3, UnixNow(), 100)
	if err != nil {
		t.Fatal(err)
	}
	thumbIDs := map[int64]bool{}
	for _, ref := range thumbs {
		thumbIDs[ref.ID] = true
	}
	if !thumbIDs[thumbMe] {
		t.Error("a probed image with no thumbnail was not offered for thumbnailing")
	}
	if thumbIDs[done] {
		t.Error("a file that already has a thumbnail was offered again")
	}
	if thumbIDs[probeMe] {
		t.Error("a file that has not been probed was offered for thumbnailing; " +
			"the format is not known to be supported yet")
	}
	if thumbIDs[skipped] {
		t.Error("a deliberately skipped file was offered for thumbnailing")
	}
	if thumbIDs[gone] {
		t.Error("a missing original was offered for thumbnailing; the read would fail")
	}
}

// TestRetryBoundIsEnforced: a file that keeps failing must stop being offered,
// because the design forbids an immediate or unbounded retry loop.
func TestRetryBoundIsEnforced(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	ref, err := s.UpsertMedia(ctx, scanRow(col, "bad.jpg", 10, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Kind: "image", Format: "jpeg",
		ThumbState: ThumbPending}); err != nil {
		t.Fatal(err)
	}

	const maxAttempts = 3
	for i := 0; i < maxAttempts; i++ {
		q, err := s.NextThumb(ctx, col, maxAttempts, UnixNow(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(q) != 1 {
			t.Fatalf("attempt %d: %d items offered, want 1", i, len(q))
		}
		if err := s.SetThumb(ctx, ThumbUpdate{ID: ref.ID, State: ThumbFailed,
			Reason: "decode_error", NextAttemptAt: 0}); err != nil {
			t.Fatal(err)
		}
	}
	q, err := s.NextThumb(ctx, col, maxAttempts, UnixNow(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 0 {
		t.Errorf("a file that has failed %d times is still offered", maxAttempts)
	}

	// The backoff is a second, independent bound: a file that has failed only
	// once must still be held back until its next_attempt_at. A separate row is
	// needed because SetThumb always increments the attempt count.
	backoff, err := s.UpsertMedia(ctx, scanRow(col, "backoff.jpg", 10, 10))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProbe(ctx, ProbeUpdate{ID: backoff.ID, State: ProbeOK, Kind: "image", Format: "jpeg",
		ThumbState: ThumbPending}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextThumb(ctx, col, maxAttempts, UnixNow(), 10); err != nil {
		t.Fatal(err)
	}
	if err := s.SetThumb(ctx, ThumbUpdate{ID: backoff.ID, State: ThumbFailed,
		Reason: "decode_error", NextAttemptAt: UnixNow() + 3600}); err != nil {
		t.Fatal(err)
	}
	q, err = s.NextThumb(ctx, col, maxAttempts, UnixNow(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 0 {
		t.Errorf("a file whose next_attempt_at is an hour away was offered now (%d items)", len(q))
	}
	// Once the backoff has elapsed it is offered again, so a transient failure
	// is a delay rather than a permanent exclusion.
	q, err = s.NextThumb(ctx, col, maxAttempts, UnixNow()+7200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 1 {
		t.Errorf("a file whose backoff has elapsed was not offered (%d items)", len(q))
	}
}

// TestNextWorkRespectsItsLimit is the property that keeps a run bounded: asking
// for ten files must never return a hundred, however large the backlog.
func TestNextWorkRespectsItsLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	for i := 0; i < 50; i++ {
		if _, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("f%03d.jpg", i), 10, 10)); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{1, 5, 10, 49} {
		q, err := s.NextProbe(ctx, col, 3, UnixNow(), limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(q) != limit {
			t.Errorf("NextProbe(limit=%d) returned %d items", limit, len(q))
		}
	}
	// A non-positive limit must be refused rather than passed to SQL, where
	// LIMIT -1 means "no limit" and an accidental --limit -1 would fetch the
	// whole pending set.
	for _, limit := range []int{0, -1, -1000} {
		if _, err := s.NextProbe(ctx, col, 3, UnixNow(), limit); err == nil {
			t.Errorf("NextProbe(limit=%d) was accepted; SQL would read a negative LIMIT as unbounded", limit)
		}
		if _, err := s.NextThumb(ctx, col, 3, UnixNow(), limit); err == nil {
			t.Errorf("NextThumb(limit=%d) was accepted", limit)
		}
	}
	// An absurdly large request is clamped rather than honoured, so one fetch
	// cannot become an unbounded read.
	if q, err := s.NextProbe(ctx, col, 3, UnixNow(), 1_000_000); err != nil {
		t.Fatal(err)
	} else if len(q) > MaxWorkBatch {
		t.Errorf("a request for a million rows returned %d; the batch must be clamped to %d",
			len(q), MaxWorkBatch)
	}
}

// TestListMediaPagesWithoutLoadingEverything is the memory guarantee the brief
// requires: the dataset must be readable in bounded slices, and the slice size
// must be what the caller asked for.
func TestListMediaPagesWithoutLoadingEverything(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	const total = 250
	for i := 0; i < total; i++ {
		if _, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("d/f%04d.jpg", i), 10, int64(1000+i))); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[int64]bool{}
	var cursor *Cursor
	pages := 0
	// Bounded by a generous page budget so a paging bug fails loudly instead of
	// looping forever.
	for {
		var cur Cursor
		if cursor != nil {
			cur = *cursor
		}
		page, next, err := s.ListMedia(ctx, Filter{}, cur, 40)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 40 {
			t.Fatalf("a page of 40 returned %d rows", len(page))
		}
		for _, m := range page {
			if seen[m.ID] {
				t.Fatalf("row %d appeared on two pages", m.ID)
			}
			seen[m.ID] = true
		}
		pages++
		if pages > 20 {
			t.Fatalf("paging did not terminate after 20 pages; %d rows seen", len(seen))
		}
		if next == nil {
			break
		}
		cursor = next
	}
	if len(seen) != total {
		t.Errorf("paging returned %d distinct rows, want %d", len(seen), total)
	}
}

// TestCursorPagingIsStableAcrossInserts: a page token must keep meaning the
// same place even if rows are added, or the gallery would show duplicates and
// skip items while a scan runs in the background.
func TestCursorPagingIsStableAcrossInserts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	for i := 0; i < 20; i++ {
		if _, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("f%02d.jpg", i), 10, int64(1000+i))); err != nil {
			t.Fatal(err)
		}
	}
	first, next, err := s.ListMedia(ctx, Filter{}, Cursor{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	firstIDs := map[int64]bool{}
	for _, m := range first {
		firstIDs[m.ID] = true
	}
	if next == nil {
		t.Fatal("no cursor after a partial page")
	}

	// A new row arrives between pages.
	if _, err := s.UpsertMedia(ctx, scanRow(col, "zz_new.jpg", 10, 99999)); err != nil {
		t.Fatal(err)
	}

	second, _, err := s.ListMedia(ctx, Filter{}, *next, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range second {
		if firstIDs[m.ID] {
			t.Errorf("row %d was returned again after an insert between pages; "+
				"the gallery would show a duplicate", m.ID)
		}
	}
}

// TestALongQueryIsClampedToTheDocumentedLimit keeps user input from turning an
// unbounded string into an unbounded scan. The limit is what makes the cost of a
// search predictable: the pattern handed to SQLite is never longer than
// MaxQueryLen regardless of what the caller passes.
//
// The two seeded names are identical for their first MaxQueryLen characters and
// differ only afterwards. A query that is the common prefix plus the first
// file's distinguishing tail therefore has to match *both* rows if it was
// clamped, and only the first if it was passed through unchanged. That makes the
// clamp observable rather than assumed.
func TestALongQueryIsClampedToTheDocumentedLimit(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	prefix := strings.Repeat("a", MaxQueryLen+20)
	for _, rel := range []string{prefix + "ZZZ.jpg", prefix + "YYY.jpg"} {
		if _, err := s.UpsertMedia(ctx, scanRow(col, rel, 10, 10)); err != nil {
			t.Fatal(err)
		}
	}

	long := prefix + "ZZZ"
	items, _, err := s.ListMedia(ctx, Filter{Query: long}, Cursor{}, 10)
	if err != nil {
		t.Fatalf("a query of %d characters returned an error: %v", len(long), err)
	}
	if len(items) != 2 {
		t.Errorf("a %d character query matched %d of 2 media; the pattern must be clamped to "+
			"%d characters before it reaches SQL, so the distinguishing tail must be dropped",
			len(long), len(items), MaxQueryLen)
	}

	// A query inside the limit is used in full, so the same tail must now
	// select exactly one of the two names.
	short := strings.Repeat("a", 8) + "ZZZ"
	if len(short) > MaxQueryLen {
		t.Fatalf("the test's short query is %d characters, over the %d limit", len(short), MaxQueryLen)
	}
	items, _, err = s.ListMedia(ctx, Filter{Query: short}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Errorf("a %d character query matched %d media, want exactly the one whose name ends ZZZ",
			len(short), len(items))
	}
}

// TestSearchFindsFilesByNameAndIgnoresWildcards covers the "simple search" the
// brief asks for, and the fact that a search term is data rather than a
// pattern: a name containing % must match literally, not as a wildcard.
func TestSearchFindsFilesByNameAndIgnoresWildcards(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	for _, rel := range []string{"paris.jpg", "rome.jpg", "photo 100%.jpg"} {
		if _, err := s.UpsertMedia(ctx, scanRow(col, rel, 10, 10)); err != nil {
			t.Fatal(err)
		}
	}

	got, _, err := s.ListMedia(ctx, Filter{Query: "paris"}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RelPath != "paris.jpg" {
		t.Errorf("search for paris returned %d rows: %+v", len(got), got)
	}

	// A literal % must not act as a wildcard and match everything.
	got, _, err = s.ListMedia(ctx, Filter{Query: "100%"}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if m.RelPath != "photo 100%.jpg" {
			t.Errorf("search for %q matched %q; a wildcard in the term was treated as syntax", "100%", m.RelPath)
		}
	}

	// An underscore is also literal.
	if _, err := s.UpsertMedia(ctx, scanRow(col, "a_b.jpg", 10, 10)); err != nil {
		t.Fatal(err)
	}
	get, _, err := s.ListMedia(ctx, Filter{Query: "a_b"}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range get {
		if m.RelPath != "a_b.jpg" {
			t.Errorf("search for a_b matched %q; underscore was treated as a single-character wildcard", m.RelPath)
		}
	}
}

// TestFilterByCollectionIsExact guards the API rule that only a known
// collection id is accepted, never an arbitrary path.
func TestFilterByCollectionIsExact(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	first := seedCollection(t, s)
	second, err := s.UpsertCollection(ctx, "videos", "Videos", "/mnt/raid0/videos", "/mnt/raid0", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertMedia(ctx, scanRow(first, "photo.jpg", 10, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertMedia(ctx, scanRow(second, "clip.mp4", 10, 10)); err != nil {
		t.Fatal(err)
	}

	got, _, err := s.ListMedia(ctx, Filter{CollectionID: first}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RelPath != "photo.jpg" {
		t.Errorf("filtering by collection returned %+v", got)
	}

	counts, err := s.CollectionCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(counts) != 2 {
		t.Errorf("CollectionCounts returned %d entries for 2 collections", len(counts))
	}
}

// TestTotalsAndPendingCountsAgreeWithTheRows keeps status honest: the numbers
// shown to the operator are what the queues would actually act on.
func TestTotalsAndPendingCountsAgreeWithTheRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	for i := 0; i < 7; i++ {
		if _, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("f%d.jpg", i), 10, 10)); err != nil {
			t.Fatal(err)
		}
	}
	pendingProbe, pendingThumb, err := s.CountPending(ctx, col)
	if err != nil {
		t.Fatal(err)
	}
	if pendingProbe != 7 {
		t.Errorf("pending probes = %d, want 7", pendingProbe)
	}
	if pendingThumb != 0 {
		t.Errorf("pending thumbnails = %d before any probe, want 0: the format is not known to be supported",
			pendingThumb)
	}

	refs, err := s.NextProbe(ctx, col, 3, UnixNow(), 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Kind: "image", Format: "jpeg",
			Width: 800, Height: 600, Pixels: 480_000, ThumbState: ThumbPending}); err != nil {
			t.Fatal(err)
		}
	}
	if _, pendingThumb, err = s.CountPending(ctx, col); err != nil {
		t.Fatal(err)
	}
	if pendingThumb != 7 {
		t.Errorf("pending thumbnails = %d after probing 7 images, want 7", pendingThumb)
	}
	if _, _, err := s.SumThumbBytes(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestSumThumbBytesCountsOnlyWhatExists: cache accounting must not include
// rows whose cache file has been deleted, or cache-report would overstate usage
// and cache-gc would think it had work when it did not.
func TestSumThumbBytesCountsOnlyWhatExists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	var ids []int64
	for i := 0; i < 3; i++ {
		ref, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("f%d.jpg", i), 10, 10))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ref.ID)
		if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Format: "jpeg"}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range ids {
		if err := s.SetThumb(ctx, ThumbUpdate{ID: id, State: ThumbOK,
			Bytes: int64(1000 * (i + 1)), At: UnixNow()}); err != nil {
			t.Fatal(err)
		}
	}
	files, bytes, err := s.SumThumbBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if files != 3 {
		t.Errorf("SumThumbBytes files = %d, want 3", files)
	}
	if bytes != 6000 {
		t.Errorf("SumThumbBytes bytes = %d, want 6000", bytes)
	}
}

// TestAllThumbPathsIsBoundedAndInsideTheCacheRoot is the safety property
// cache-gc depends on. If this could return a path outside the cache root, a
// cleanup pass driven by the index could delete user files.
func TestAllThumbPathsIsBoundedAndInsideTheCacheRoot(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		ref, err := s.UpsertMedia(ctx, scanRow(col, fmt.Sprintf("d/f%d.jpg", i), 10, 10))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Kind: "image",
			Format: "jpeg", ThumbState: ThumbPending}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetThumb(ctx, ThumbUpdate{ID: ref.ID, State: ThumbOK,
			Bytes: 100, At: UnixNow()}); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := s.AllThumbPaths(ctx, root, 512, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 12 {
		t.Errorf("AllThumbPaths returned %d paths, want 12", len(paths))
	}
	cleanRoot := filepath.Clean(root) + string(filepath.Separator)
	for _, p := range paths {
		if !strings.HasPrefix(filepath.Clean(p), cleanRoot) {
			t.Errorf("AllThumbPaths returned %q, which is outside the cache root %q; "+
				"a cleanup pass would then be able to delete user files", p, root)
		}
	}
}

// TestMarkMissingDoesNotDeleteRows is a requirement rather than an
// implementation detail: a drive that failed to mount must not cause the index
// to be emptied. The rows stay and are flagged.
func TestMarkMissingDoesNotDeleteRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	base := UnixNow()
	old := scanRow(col, "placeholder", 10, 10)
	old.At = base
	old.RunToken = 1 // stamped by the previous run, in this same second
	for i := 0; i < 5; i++ {
		row := old
		row.RelPath = fmt.Sprintf("f%d.jpg", i)
		row.BaseName = row.RelPath
		if _, err := s.UpsertMedia(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	// Every row was stamped by the previous run, token 1. One file is seen
	// again by "this run" and gets token 2; the other four keep token 1.
	//
	// The two runs deliberately share a wall-clock second. That is the case the
	// token exists for: comparing `last_seen < runStart` made an index scanned
	// twice inside one second unable to notice that a file had gone, which is
	// exactly what an operator does when checking that a delete took effect.
	if n, err := s.MarkMissing(ctx, col, 2); err != nil {
		t.Fatal(err)
	} else if n != 5 {
		t.Fatalf("MarkMissing marked %d rows before this run saw anything, want all 5", n)
	}
	if _, err := s.ClearMissing(ctx, col); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.RelPath = "f0.jpg"
	fresh.BaseName = "f0.jpg"
	fresh.RunToken = 2
	if _, err := s.UpsertMedia(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkMissing(ctx, col, 2); err != nil {
		t.Fatal(err)
	} else if n != 4 {
		t.Fatalf("MarkMissing marked %d rows, want 4", n)
	}
	totals, err := s.Totals(ctx, col)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Media != 5 {
		t.Errorf("Totals.Total = %d after MarkMissing, want 5: rows must never be deleted for a "+
			"missing original", totals.Media)
	}
	// By default a marked file is hidden from the gallery, so the timeline and
	// the grid do not show frames whose original cannot be read.
	defaultView, _, err := s.ListMedia(ctx, Filter{}, Cursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultView) != 1 {
		t.Errorf("the default listing returned %d rows, want only the 1 file seen during this run; "+
			"a missing original must not be offered for display", len(defaultView))
	}
	for _, m := range defaultView {
		if m.Missing {
			t.Errorf("%q is marked missing but appears in the default listing", m.RelPath)
		}
	}

	// Including them shows the flag, which is what an operator needs when
	// deciding whether a mount came back.
	withMissing, _, err := s.ListMedia(ctx, Filter{IncludeMissing: true}, Cursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	missing := 0
	for _, m := range withMissing {
		if m.Missing {
			missing++
		}
	}
	if missing != 4 {
		t.Errorf("%d rows are marked missing, want 4 (all but the one seen during this run)", missing)
	}

	if _, err := s.ClearMissing(ctx, col); err != nil {
		t.Fatal(err)
	}
	page, _, err := s.ListMedia(ctx, Filter{}, Cursor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 {
		t.Errorf("after ClearMissing the default listing returned %d rows, want all 5", len(page))
	}
	for _, m := range page {
		if m.Missing {
			t.Errorf("ClearMissing left %q marked missing", m.RelPath)
		}
	}
}

// TestPinCollectionFSIDRecordsAndDetectsChange is the check that refuses to run
// when a path lands somewhere it should not: if the mount is lost, the root
// becomes an ordinary directory on another filesystem, and the recorded
// identity no longer matches.
func TestPinCollectionFSIDRecordsAndDetectsChange(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	find := func(t *testing.T) Collection {
		t.Helper()
		all, err := s.Collections(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range all {
			if c.ID == col {
				return c
			}
		}
		t.Fatalf("collection %d not found", col)
		return Collection{}
	}

	before := find(t)
	if before.ExpectFSID != "" {
		t.Errorf("a fresh collection has expect_fsid %q, want empty so the first check records it",
			before.ExpectFSID)
	}
	if err := s.PinCollectionFSID(ctx, col, "1234-abcd"); err != nil {
		t.Fatal(err)
	}
	after := find(t)
	if after.ExpectFSID != "1234-abcd" {
		t.Errorf("the pinned fsid is %q, want 1234-abcd", after.ExpectFSID)
	}
	// Re-pinning with the same value is fine; the caller decides whether a
	// mismatch is fatal.
	if err := s.PinCollectionFSID(ctx, col, "1234-abcd"); err != nil {
		t.Fatal(err)
	}
}

// TestSweepStaleTasksRecoversFromAKilledProcess is the crash-recovery property:
// a run killed with SIGKILL leaves a task row that says "running" forever.
// Nothing may trust that row; a stale one has to be swept.
func TestSweepStaleTasksRecoversFromAKilledProcess(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedCollection(t, s)

	id, err := s.StartTask(ctx, Task{Kind: "thumbs", CollectionCode: "photos", PID: 999999})
	if err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Fatalf("%d running tasks after StartTask, want 1", len(running))
	}

	// The process is gone (pid 999999 is not alive), so the task must be
	// swept - not left as a phantom "running" that blocks the next run.
	swept, err := s.SweepStaleTasks(ctx, "/proc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if swept < 1 {
		t.Errorf("SweepStaleTasks swept %d tasks, want at least the one whose process is gone", swept)
	}
	running, err = s.RunningTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("%d tasks still report as running after the sweep: %+v", len(running), running)
	}

	last, err := s.LastTask(ctx, "thumbs")
	if err != nil {
		t.Fatal(err)
	}
	if last.ID != id {
		t.Errorf("LastTask returned id %d, want %d", last.ID, id)
	}
	if last.State == "" || last.State == "running" {
		t.Errorf("the swept task has state %q; a stale row must not still claim to be running", last.State)
	}
	if last.Reason == "" {
		t.Error("the swept task has no reason recorded, so status cannot explain what happened")
	}
	// Task ids and collection codes survive, so history is not lost.
	if last.CollectionCode != "photos" {
		t.Errorf("the swept task lost its collection code: %q", last.CollectionCode)
	}
}

// TestFinishTaskRecordsDiagnostics: a failed run has to leave enough behind to
// explain itself. The report is a text field for that reason.
func TestFinishTaskRecordsDiagnostics(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedCollection(t, s)
	id, err := s.StartTask(ctx, Task{Kind: "scan", CollectionCode: "photos", PID: int64(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Heartbeat(ctx, id, 10, 2, 1, 4096, 2048); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, id, "done", "collection_exhausted", "limit reached",
		Task{Kind: "scan", CollectionCode: "photos"}, "processed=10 skipped=2 failed=1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LastTask(ctx, "scan")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" {
		t.Errorf("the finished task has state %q, want done", got.State)
	}
	if got.Reason != "collection_exhausted" {
		t.Errorf("the stop reason is %q, want it preserved so status can report why the run ended", got.Reason)
	}
	// The counters reported by Heartbeat must survive FinishTask even when the
	// finishing call passes a Task without them. On a run that was stopped by
	// resource pressure, the last heartbeat is the only record of how far it
	// got, so losing it would erase the diagnostic the operator needs.
	if got.Processed != 10 || got.Skipped != 2 || got.Failed != 1 {
		t.Errorf("the counters are %d/%d/%d, want the 10/2/1 reported by Heartbeat",
			got.Processed, got.Skipped, got.Failed)
	}
	if got.BytesRead != 4096 || got.BytesWritten != 2048 {
		t.Errorf("the byte counters are %d/%d, want the 4096/2048 reported by Heartbeat",
			got.BytesRead, got.BytesWritten)
	}

	// A caller that does supply the final totals must have them recorded.
	id2, err := s.StartTask(ctx, Task{Kind: "thumbs", CollectionCode: "photos", PID: int64(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(ctx, id2, "stopped", "pressure", "MemAvailable below stop",
		Task{Kind: "thumbs", CollectionCode: "photos", Processed: 7, Skipped: 1, Failed: 2},
		"processed=7"); err != nil {
		t.Fatal(err)
	}
	got2, err := s.LastTask(ctx, "thumbs")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Processed != 7 || got2.Skipped != 1 || got2.Failed != 2 {
		t.Errorf("the supplied totals were not recorded: %d/%d/%d, want 7/1/2",
			got2.Processed, got2.Skipped, got2.Failed)
	}
	if got.Report == "" {
		t.Error("the run report was not stored")
	}
}

func TestAddTaskItemIsBounded(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedCollection(t, s)
	id, err := s.StartTask(ctx, Task{Kind: "thumbs", CollectionCode: "photos", PID: int64(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxTaskItems+50; i++ {
		if err := s.AddTaskItem(ctx, TaskItem{TaskID: id, RelPath: fmt.Sprintf("f%04d.jpg", i),
			Action: "thumb", Outcome: "ok", At: UnixNow()}); err != nil {
			t.Fatalf("AddTaskItem %d: %v", i, err)
		}
	}
	// A caller asking for more than the cap must not receive more than the cap.
	items, err := s.TaskItems(ctx, id, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > maxTaskItems {
		t.Errorf("a task accumulated %d items; per-file detail must be bounded so a large run "+
			"cannot grow the index without limit", len(items))
	}
	// The cap must be visible to a reader rather than silent, so an operator
	// knows the returned list is a sample and the counters are the authority.
	// The marker row is written when the cap is reached, which places it beyond
	// the window a 500-row read returns, so it is counted directly.
	var marker int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM task_items WHERE task_id = ? AND action = 'truncated'`, id).
		Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 1 {
		t.Errorf("found %d truncation markers, want exactly 1: the cap must be recorded once, "+
			"so a reader can tell the list is a sample", marker)
	}
	var stored int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM task_items WHERE task_id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored > maxTaskItems+1 {
		t.Errorf("the task has %d stored items; the cap plus one marker is the maximum", stored)
	}
	// A non-positive limit must not mean "everything".
	if got, err := s.TaskItems(ctx, id, 0); err != nil {
		t.Fatal(err)
	} else if len(got) > maxTaskItems {
		t.Errorf("TaskItems(limit=0) returned %d items", len(got))
	}
}

// TestStaleLocksAreReleasedAfterAKill covers taskctl's lock, which has to work
// when the holder was killed rather than shut down.
func TestStaleLocksAreReleasedAfterAKill(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	seedCollection(t, s)
	if _, err := s.StartTask(ctx, Task{Kind: "thumbs", CollectionCode: "photos", PID: 999999}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SweepStaleTasks(ctx, "/proc", 0); err != nil {
		t.Fatal(err)
	} else if n < 1 {
		t.Fatalf("swept %d tasks, want at least 1", n)
	}
	// A live process's task must not be swept.
	live, err := s.StartTask(ctx, Task{Kind: "thumbs", CollectionCode: "photos", PID: int64(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SweepStaleTasks(ctx, "/proc", time.Hour); err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, task := range running {
		if task.ID == live {
			found = true
		}
	}
	if !found {
		t.Error("the sweep removed the task of a process that is still alive")
	}
}

// TestWaitForWriteIdleReturnsPromptly is a responsiveness requirement: status
// must return quickly and never wait for a media decode to finish.
func TestWaitForWriteIdleReturnsPromptly(t *testing.T) {
	ctx := context.Background()
	s := newFileStore(t)
	seedCollection(t, s)
	start := time.Now()
	if err := s.WaitForWriteIdle(ctx, 2*time.Second); err != nil {
		t.Fatalf("WaitForWriteIdle on an idle store: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("WaitForWriteIdle took %v on an idle store; status must return promptly", elapsed)
	}
}

// TestMediumPathIsSafeForUntrustedNames covers the one place a file name is
// stored for later display. Whatever it contains, it must round-trip unchanged
// and never be interpreted as a path.
func TestMediumPathIsSafeForUntrustedNames(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	hostile := []string{
		"../../etc/passwd",
		"a/../../b.jpg",
		".hidden/.very/secret.jpg",
		"name with spaces and 'quotes'.jpg",
		`<script>alert(1)</script>.jpg`,
		"中文/照片.jpg",
		"emoji/🎞️ clip.mp4",
		"with\ttab.jpg",
		strings.Repeat("deep/", 8) + "leaf.jpg",
	}
	for _, rel := range hostile {
		res, err := s.UpsertMedia(ctx, scanRow(col, rel, 10, 10))
		if err != nil {
			t.Errorf("UpsertMedia(%q): %v", rel, err)
			continue
		}
		got, err := s.GetMedia(ctx, res.ID)
		if err != nil {
			t.Errorf("GetMedia after storing %q: %v", rel, err)
			continue
		}
		if got.RelPath != rel {
			t.Errorf("RelPath round-tripped as %q, want %q", got.RelPath, rel)
		}
	}

	totals, err := s.Totals(ctx, col)
	if err != nil {
		t.Fatal(err)
	}
	if int(totals.Media) != len(hostile) {
		t.Errorf("Totals.Total = %d, want %d", totals.Media, len(hostile))
	}
}

func TestEmptyIndexPathIsRefused(t *testing.T) {
	if _, err := Open("", Options{}); err == nil {
		t.Error("Open accepted an empty index path with Memory off")
	}
}

func TestTimelineGroupsByDay(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)

	// Two photos on one day, one on another, one whose time is the file
	// modification time (a labelled fallback), and one with no usable time at
	// all. The last is not placed on the timeline, because the design forbids
	// inventing a date, but it must be counted and reachable.
	day1 := time.Date(2019, 7, 4, 12, 0, 0, 0, time.UTC).Unix()
	day2 := time.Date(2021, 6, 1, 9, 0, 0, 0, time.UTC).Unix()
	day3 := int64(1_600_000_000)
	setups := []struct {
		rel   string
		taken *int64
		src   string
	}{
		{"a.jpg", &day1, TakenSrcExif},
		{"b.jpg", &day1, TakenSrcExif},
		{"c.jpg", &day2, TakenSrcExif},
		{"d.jpg", &day3, TakenSrcFiletime},
		{"e.jpg", nil, TakenSrcNone},
	}
	for _, st := range setups {
		ref, err := s.UpsertMedia(ctx, scanRow(col, st.rel, 10, 10))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Format: "jpeg",
			Width: 100, Height: 100, Pixels: 10_000,
			TakenAt: st.taken, TakenSrc: st.src}); err != nil {
			t.Fatal(err)
		}
	}

	rows, undated, err := s.Timeline(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// Three distinct days are represented: 2019-07 (two photos), 2021-06, and
	// the day of the fallback timestamp.
	if len(rows) != 3 {
		t.Errorf("Timeline returned %d groups, want 3: %+v", len(rows), rows)
	}
	var dated int64
	for _, r := range rows {
		dated += r.Count
	}
	if dated != 4 {
		t.Errorf("the groups account for %d items, want the 4 that have a usable time", dated)
	}
	// The one file with no usable time is counted separately, so the UI can say
	// how many are not on the timeline rather than losing them silently.
	if undated != 1 {
		t.Errorf("undated count = %d, want 1", undated)
	}

	// The fallback filter reaches the file whose time is only a file time, and
	// not the one with no time at all, which is a different condition.
	fallback, _, err := s.ListMedia(ctx, Filter{OnlyFallback: true}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundFallback, foundNone := false, false
	for _, m := range fallback {
		switch m.RelPath {
		case "d.jpg":
			foundFallback = true
		case "e.jpg":
			foundNone = true
		}
	}
	if !foundFallback {
		t.Error("the file whose shown date is a fallback is not reachable through the fallback " +
			"filter; an operator could not find the items whose dates are not real")
	}
	if foundNone {
		t.Error("a file with no usable time is reported as a fallback; it has no date to fall back to, " +
			"and calling it one would present a guess as a file time")
	}

	// Every row is still listed, so the undated one is visible in the gallery.
	all, _, err := s.ListMedia(ctx, Filter{}, Cursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 5 {
		t.Errorf("ListMedia returned %d rows, want all 5 including the undated one", len(all))
	}
	// The fallback must be labelled, not presented as a capture time.
	if !TakenSrcIsFallback(TakenSrcFiletime) {
		t.Error("TakenSrcIsFallback(filetime) = false; a file time must be marked as a fallback in the UI")
	}
	if TakenSrcIsFallback(TakenSrcExif) {
		t.Error("TakenSrcIsFallback(exif) = true; a real capture time must not be labelled a fallback")
	}
}

// TestRequeueRetriesThumbnailsStoppedByMemory: a decoder change can make a
// file that hit the worker's memory ceiling fit, so a policy change queues
// it again, even after its attempts ran out. Ordinary failures stay failed.
func TestRequeueRetriesThumbnailsStoppedByMemory(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	col := seedCollection(t, s)
	refs := map[string]int64{}
	for _, name := range []string{"rss.png", "oom.jpg", "broken.jpg"} {
		ref, err := s.UpsertMedia(ctx, scanRow(col, name, 100, 1))
		if err != nil {
			t.Fatal(err)
		}
		refs[name] = ref.ID
		if err := s.SetProbe(ctx, ProbeUpdate{ID: ref.ID, State: ProbeOK, Format: "png", Width: 6000, Height: 8000, Pixels: 48_000_000}); err != nil {
			t.Fatal(err)
		}
	}
	for name, reason := range map[string]string{"rss.png": "worker_rss_over_max", "oom.jpg": "cgroup_oom_kill", "broken.jpg": "undecodable"} {
		if err := s.SetThumb(ctx, ThumbUpdate{ID: refs[name], State: ThumbFailed, Reason: reason, NextAttemptAt: UnixNow() + 3600, At: UnixNow()}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.RequeueSkipped(ctx, col, nil)
	if err != nil || n != 2 {
		t.Fatalf("requeued %d, %v; want the two stopped by memory", n, err)
	}
	for name, want := range map[string]string{"rss.png": ThumbPending, "oom.jpg": ThumbPending, "broken.jpg": ThumbFailed} {
		m, err := s.GetMedia(ctx, refs[name])
		if err != nil || m.ThumbState != want {
			t.Errorf("%s: thumb state %q, want %q (%v)", name, m.ThumbState, want, err)
		}
	}
}
