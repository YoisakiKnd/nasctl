package suzu

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// buildDB creates a suzu-shaped database for the tests. The schema follows the
// real one closely enough to exercise the code: the point is to test this
// package's reading and containment, not suzu itself.
func buildDB(t *testing.T, dir string, extra func(*sql.DB)) string {
	t.Helper()
	path := filepath.Join(dir, "suzu.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE library (id TEXT PRIMARY KEY, title TEXT, path TEXT, size INTEGER, added_at INTEGER)`,
		`CREATE TABLE subscriptions (id TEXT PRIMARY KEY, title TEXT)`,
		`CREATE TABLE items (id TEXT PRIMARY KEY, title TEXT)`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, state TEXT, done_bytes INTEGER, total_bytes INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		extra(db)
	}
	return path
}

func insertLibrary(t *testing.T, path string, rows [][3]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO library(id,title,path,size,added_at) VALUES(?,?,?,?,?)`,
			r[0], r[1], r[2], 100*(i+1), 1700000000+i); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAnUnconfiguredIntegrationSaysSo: the console must not invent a library.
func TestAnUnconfiguredIntegrationSaysSo(t *testing.T) {
	if _, err := Open(Config{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	// A nil client reports unconfigured rather than panicking.
	var c *Client
	if c.Configured() {
		t.Error("a nil client reports itself configured")
	}
}

// TestAMissingDatabaseIsReportedNotCreated: opening read-only must not create the
// file. Creating it would leave an empty database that then looks like an empty
// library.
func TestAMissingDatabaseIsReportedNotCreated(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.db")
	_, err := Open(Config{DBPath: missing})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v, want ErrUnavailable", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Error("opening a missing database created the file")
	}
}

// TestAnUnexpectedSchemaIsRefused: a different database must be reported, not read
// as if it were suzu's.
func TestAnUnexpectedSchemaIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "other.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err = Open(Config{DBPath: path})
	if !errors.Is(err, ErrSchema) {
		t.Fatalf("error = %v, want ErrSchema", err)
	}
	if !strings.Contains(err.Error(), "library") {
		t.Errorf("the error does not name the missing table: %v", err)
	}
}

// TestTheDatabaseIsOpenedReadOnly: the console must not be able to write suzu's
// state. This is the property that keeps two writers away from one library.
func TestTheDatabaseIsOpenedReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, nil)
	c, err := Open(Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.db.Exec(`INSERT INTO library(id,title,path) VALUES('x','x','x')`)
	if err == nil {
		t.Fatal("a write to suzu's database succeeded; the console must not be a second writer")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "read-only") {
		t.Logf("the write failed with: %v (acceptable, but check it is a read-only refusal)", err)
	}

	// And the row count is unchanged.
	var n int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM library`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("library has %d rows after a refused write, want 0", n)
	}
}

func TestLibraryReadsAndPages(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, nil)
	rows := make([][3]string, 0, 12)
	for i := 0; i < 12; i++ {
		rows = append(rows, [3]string{
			fmt.Sprintf("ep%02d", i), fmt.Sprintf("Episode %02d", i),
			fmt.Sprintf("/library/ep%02d.mkv", i),
		})
	}
	insertLibrary(t, path, rows)

	c, err := Open(Config{DBPath: path, LibraryRoot: "/library"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	page, total, err := c.Library(context.Background(), "", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Errorf("total = %d, want 12", total)
	}
	if len(page) != 5 {
		t.Errorf("page = %d rows, want 5: the query must be paged in SQL, not in Go", len(page))
	}
	if page[0].Title != "Episode 00" {
		t.Errorf("first title = %q", page[0].Title)
	}
	if page[0].Size == 0 {
		t.Error("size was not read")
	}
	if page[0].Added.IsZero() {
		t.Error("added time was not read")
	}

	// A second page does not repeat the first.
	page2, _, err := c.Library(context.Background(), "", 5, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) == 0 {
		t.Fatal("page 2 is empty")
	}
	if page2[0].ID == page[0].ID {
		t.Errorf("page 2 repeats page 1's first row (%s)", page2[0].ID)
	}
}

func TestLibrarySearchEscapesWildcards(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, nil)
	insertLibrary(t, path, [][3]string{
		{"a", "100% Cotton", "/library/a.mkv"},
		{"b", "Plain Title", "/library/b.mkv"},
	})
	c, err := Open(Config{DBPath: path, LibraryRoot: "/library"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A query containing % must match a literal percent sign, not everything.
	got, total, err := c.Library(context.Background(), "%", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(got) != 1 || got[0].ID != "a" {
		t.Errorf("searching for %% returned %d rows (total %d); a wildcard must be literal",
			len(got), total)
	}

	// An underscore is likewise literal.
	if _, total, err := c.Library(context.Background(), "_", 10, 0); err != nil {
		t.Fatal(err)
	} else if total != 0 {
		t.Errorf("searching for _ matched %d rows; it must be literal", total)
	}
}

// TestPlaybackIsRefusedOutsideTheLibraryRoot is the containment test. A path
// recorded in suzu's database must not let the console read an arbitrary file: the
// database is data, and data does not get to name what the console opens.
func TestPlaybackIsRefusedOutsideTheLibraryRoot(t *testing.T) {
	base := t.TempDir()
	libRoot := filepath.Join(base, "library")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{libRoot, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(libRoot, "ep01.mkv")
	if err := os.WriteFile(inside, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := buildDB(t, base, nil)
	insertLibrary(t, path, [][3]string{
		{"good", "Inside", inside},
		{"escape", "Escape", secret},
		{"dotdot", "Traversal", filepath.Join(libRoot, "..", "outside", "secret.txt")},
		{"rel", "Relative", "ep01.mkv"},
		{"missing", "Missing", filepath.Join(libRoot, "gone.mkv")},
	})

	c, err := Open(Config{DBPath: path, LibraryRoot: libRoot})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	items, _, err := c.Library(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]LibraryItem{}
	for _, it := range items {
		byID[it.ID] = it
	}

	if !byID["good"].Playable {
		t.Errorf("a file inside the root is not playable: %s", byID["good"].Note)
	}
	if !byID["rel"].Playable {
		t.Errorf("a relative path inside the root is not playable: %s", byID["rel"].Note)
	}
	for _, id := range []string{"escape", "dotdot"} {
		if byID[id].Playable {
			t.Errorf("%s is playable; a path outside the library root must never be", id)
		}
		if byID[id].Note == "" {
			t.Errorf("%s carries no explanation for why it is not playable", id)
		}
	}
	if byID["missing"].Playable {
		t.Error("a row whose file is absent is offered as playable")
	}
	if !strings.Contains(byID["missing"].Note, "not present") {
		t.Errorf("the missing file's note does not say so: %q", byID["missing"].Note)
	}
}

// TestNoLibraryRootMeansNoPlayback: without a root the console cannot locate a
// file, and it must say that rather than guessing.
func TestNoLibraryRootMeansNoPlayback(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, nil)
	insertLibrary(t, path, [][3]string{{"a", "A", "/library/a.mkv"}})
	c, err := Open(Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	items, _, err := c.Library(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Playable {
		t.Error("a row is playable with no library_root configured")
	}
	if !strings.Contains(items[0].Note, "library_root") {
		t.Errorf("the note does not name the missing setting: %q", items[0].Note)
	}
}

func TestATitleFallsBackToTheFilename(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, nil)
	db, _ := sql.Open("sqlite", "file:"+path)
	if _, err := db.Exec(`INSERT INTO library(id,title,path) VALUES('a','','/library/Some Episode.mkv')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c, err := Open(Config{DBPath: path, LibraryRoot: "/library"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	items, _, err := c.Library(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].Title != "Some Episode.mkv" {
		t.Errorf("title = %q, want the filename as a fallback", items[0].Title)
	}
}

func TestSubscriptionsAndTasksRead(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, func(db *sql.DB) {
		db.Exec(`INSERT INTO subscriptions(id,title) VALUES('s1','Show One'),('s2','Show Two')`)
		db.Exec(`INSERT INTO tasks(id,state,done_bytes,total_bytes) VALUES('t1','running',50,100)`)
	})
	c, err := Open(Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	subs, total, err := c.Subscriptions(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(subs) != 2 {
		t.Errorf("subscriptions = %d (total %d), want 2", len(subs), total)
	}
	if subs[0].Title != "Show One" {
		t.Errorf("title = %q", subs[0].Title)
	}

	tasks, err := c.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(tasks))
	}
	if tasks[0].State != "running" || tasks[0].Progress != 50 || tasks[0].Total != 100 {
		t.Errorf("task = %+v", tasks[0])
	}
}

// TestStatusReportsWhatWasFound: the status must describe the real database, and
// must not report an unreadable count as zero.
func TestStatusReportsWhatWasFound(t *testing.T) {
	dir := t.TempDir()
	path := buildDB(t, dir, func(db *sql.DB) {
		db.Exec(`INSERT INTO library(id,title,path) VALUES('a','A','/library/a.mkv')`)
		db.Exec(`INSERT INTO subscriptions(id,title) VALUES('s','S')`)
	})
	c, err := Open(Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	st := c.Status(context.Background())
	if !st.Configured {
		t.Error("status says unconfigured")
	}
	if st.LibraryCount == nil || *st.LibraryCount != 1 {
		t.Errorf("library count = %v, want 1", st.LibraryCount)
	}
	if st.SubscriptionCount == nil || *st.SubscriptionCount != 1 {
		t.Errorf("subscription count = %v, want 1", st.SubscriptionCount)
	}
	if len(st.Tables) != 4 {
		t.Errorf("tables = %v, want the four this build reads", st.Tables)
	}
	if !strings.Contains(st.Note, "read-only") {
		t.Errorf("the note does not state that the access is read-only: %q", st.Note)
	}
	if !strings.Contains(st.Note, "library_root") {
		t.Errorf("the note does not mention the disabled playback: %q", st.Note)
	}
}

// TestAnAdaptedSchemaIsRead: this build picks the columns it recognises, so a
// renamed column is handled rather than crashing. The behaviour is tested because
// it is what makes "unexpected schema" a specific message rather than a panic.
func TestAnAdaptedSchemaIsRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suzu.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// "name" instead of "title", and no size column at all.
	stmts := []string{
		`CREATE TABLE library (id TEXT PRIMARY KEY, name TEXT, file_path TEXT, added_at INTEGER)`,
		`CREATE TABLE subscriptions (id TEXT PRIMARY KEY, name TEXT)`,
		`CREATE TABLE items (id TEXT PRIMARY KEY)`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, status TEXT, progress INTEGER, total INTEGER)`,
		`INSERT INTO library(id,name,file_path,added_at) VALUES('a','Renamed','/lib/a.mkv',1700000000)`,
		`INSERT INTO subscriptions(id,name) VALUES('s','Sub')`,
		`INSERT INTO tasks(id,status,progress,total) VALUES('t','running',1,2)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	c, err := Open(Config{DBPath: path})
	if err != nil {
		t.Fatalf("a schema with renamed columns was refused: %v", err)
	}
	defer c.Close()

	items, _, err := c.Library(context.Background(), "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "Renamed" {
		t.Errorf("items = %+v, want the renamed column read", items)
	}
	// A missing size column is unknown, not zero: reporting 0 B would be a claim.
	if items[0].Size != 0 {
		t.Errorf("size = %d", items[0].Size)
	}
	subs, _, err := c.Subscriptions(context.Background(), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Title != "Sub" {
		t.Errorf("subscriptions = %+v", subs)
	}
	tasks, err := c.Tasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].State != "running" || tasks[0].Progress != 1 {
		t.Errorf("tasks = %+v", tasks)
	}
}

func TestEscapeLikeIsLiteral(t *testing.T) {
	cases := map[string]string{
		"plain":   "plain",
		"100%":    `100\%`,
		"a_b":     `a\_b`,
		`back\sl`: `back\\sl`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeysAreSorted(t *testing.T) {
	got := keys(map[string]bool{"c": true, "a": true, "b": true})
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("keys = %v, want %v (an unsorted list makes an error message unstable)",
			got, want)
	}
}
