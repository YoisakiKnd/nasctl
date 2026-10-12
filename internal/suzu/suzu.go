// Package suzu integrates the console with the existing IWantWatchAnime service.
//
// Two rules shape everything here:
//
//   - **The database is opened read-only.** suzu is the only writer of its own
//     state. A second writer, however careful, is how a library gets corrupted,
//     and the console has nothing to gain from it: writes go through suzu's own
//     API when that is delivered, so that suzu's own invariants still apply.
//   - **Nothing is guessed.** If the database is missing, unreadable or has an
//     unexpected schema, the integration reports that and disables itself. It does
//     not fall back to scanning the filesystem for video files, because a library
//     invented from filenames is not the library suzu knows about, and presenting
//     it as one would be a fabrication.
//
// The schema is read rather than assumed: this package queries the tables and
// columns it needs and reports what it found. If suzu's schema changes, the
// console says so instead of returning wrong answers.
package suzu

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Errors returned by this package.
var (
	// ErrNotConfigured means no suzu database was configured.
	ErrNotConfigured = errors.New("suzu: not configured")
	// ErrUnavailable means the database exists but cannot be read.
	ErrUnavailable = errors.New("suzu: database unavailable")
	// ErrSchema means the database does not have the tables this build expects.
	ErrSchema = errors.New("suzu: unexpected schema")
)

// Config configures the integration.
type Config struct {
	DBPath      string
	APIBase     string
	Token       string
	LibraryRoot string
}

// Client reads suzu's state.
type Client struct {
	cfg Config
	db  *sql.DB

	// schema caches what the tables actually contain, so a query is never built
	// against a column that does not exist.
	columns map[string]map[string]bool
}

// Open opens suzu's database read-only.
func Open(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.DBPath) == "" {
		return nil, ErrNotConfigured
	}
	abs, err := filepath.Abs(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	// A read-only open must not create the file. Checking first means a missing
	// database is reported as missing rather than silently created as an empty
	// one, which would then look like an empty library.
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, abs, err)
	}

	// mode=ro is what makes this read-only at the driver level; _query_only is a
	// second, independent guard that refuses writes even if the file mode
	// permits them.
	dsn := "file:" + url.PathEscape(abs) + "?mode=ro&_query_only=true&_pragma=busy_timeout(3000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection, and no attempt to open the file for writing.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: %s: %v", ErrUnavailable, abs, err)
	}

	c := &Client{cfg: cfg, db: db}
	if err := c.loadSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

// Close releases the database.
func (c *Client) Close() error { return c.db.Close() }

// Configured reports whether the integration is set up.
func (c *Client) Configured() bool { return c != nil && c.cfg.DBPath != "" }

// LibraryRoot returns the configured archive root, if any.
func (c *Client) LibraryRoot() string { return c.cfg.LibraryRoot }

// requiredTables are the tables this integration reads. The list is what makes
// "unexpected schema" a concrete statement rather than a guess.
var requiredTables = []string{"library", "subscriptions", "items", "tasks"}

// loadSchema reads the table and column names.
func (c *Client) loadSchema(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSchema, err)
	}
	tables := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		tables[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	var missing []string
	for _, t := range requiredTables {
		if !tables[t] {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s is missing the table(s) %s; this build expects suzu's "+
			"schema, so either this is a different database or suzu has changed",
			ErrSchema, c.cfg.DBPath, strings.Join(missing, ", "))
	}

	c.columns = map[string]map[string]bool{}
	for _, t := range requiredTables {
		cols := map[string]bool{}
		crows, err := c.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t)
		if err != nil {
			return fmt.Errorf("%w: cannot read the columns of %s: %v", ErrSchema, t, err)
		}
		for crows.Next() {
			var n string
			if err := crows.Scan(&n); err != nil {
				crows.Close()
				return err
			}
			cols[n] = true
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return err
		}
		c.columns[t] = cols
	}
	return nil
}

// has reports whether a table has a column.
func (c *Client) has(table, column string) bool {
	t, ok := c.columns[table]
	if !ok {
		return false
	}
	return t[column]
}

// pick returns the first column name that exists, so that a query can adapt to
// the schema rather than failing on a renamed column.
func (c *Client) pick(table string, candidates ...string) string {
	for _, cand := range candidates {
		if c.has(table, cand) {
			return cand
		}
	}
	return ""
}

// LibraryItem is one archived episode.
type LibraryItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Path  string `json:"path"`
	// Size is zero when the schema does not record it; the UI shows "unknown"
	// rather than 0 B, because those are different facts.
	Size int64 `json:"size"`
	// Added is zero when unknown.
	Added time.Time `json:"added,omitempty"`
	// Playable reports whether a file could be resolved for playback. It is
	// false when no library_root is configured, and the UI says why.
	Playable bool   `json:"playable"`
	Note     string `json:"note,omitempty"`
}

// Library returns a page of archived episodes.
//
// The query is paged in SQL, not in Go: reading every row and slicing would make
// memory grow with the library, which is the property this project exists to
// avoid.
func (c *Client) Library(ctx context.Context, query string, limit, offset int) ([]LibraryItem, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	table := "library"
	idCol := c.pick(table, "id", "item_id")
	titleCol := c.pick(table, "title", "name")
	pathCol := c.pick(table, "path", "file_path")
	sizeCol := c.pick(table, "size", "bytes", "size_bytes")
	addedCol := c.pick(table, "added_at", "created_at", "downloaded_at")

	if idCol == "" || pathCol == "" {
		return nil, 0, fmt.Errorf("%w: the library table has no id and path columns this "+
			"build recognises; columns present: %v", ErrSchema, keys(c.columns[table]))
	}

	var (
		where []string
		args  []any
	)
	if strings.TrimSpace(query) != "" && titleCol != "" {
		where = append(where, "LOWER("+titleCol+") LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escapeLike(strings.ToLower(strings.TrimSpace(query)))+"%")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	countSQL := "SELECT COUNT(*) FROM " + table + whereSQL
	if err := c.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	cols := []string{idCol, pathCol}
	selects := []string{idCol, pathCol}
	for _, optional := range []string{titleCol, sizeCol, addedCol} {
		if optional != "" {
			selects = append(selects, optional)
			cols = append(cols, optional)
		}
	}

	q := "SELECT " + strings.Join(selects, ", ") + " FROM " + table + whereSQL +
		" ORDER BY " + idCol + " LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]LibraryItem, 0, limit)
	for rows.Next() {
		var (
			id, path string
			title    sql.NullString
			size     sql.NullInt64
			added    sql.NullInt64
		)
		dest := []any{&id, &path}
		for _, name := range cols[2:] {
			switch name {
			case titleCol:
				dest = append(dest, &title)
			case sizeCol:
				dest = append(dest, &size)
			case addedCol:
				dest = append(dest, &added)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		it := LibraryItem{ID: id, Path: path, Title: title.String, Size: size.Int64}
		if it.Title == "" {
			it.Title = filepath.Base(path)
		}
		if added.Valid && added.Int64 > 0 {
			it.Added = time.Unix(added.Int64, 0)
		}
		it.Playable, it.Note = c.playable(path)
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// playable decides whether a library row can be streamed, and says why not.
//
// The decision is deliberately conservative. A row's stored path is resolved
// against library_root and the result must stay inside it, so a path in suzu's
// database cannot be used to read an arbitrary file. Existence is checked here
// rather than assumed, because a library row whose file is gone should be shown
// as missing, not offered as a play button that fails.
func (c *Client) playable(stored string) (bool, string) {
	if c.cfg.LibraryRoot == "" {
		return false, "no library_root is configured, so files cannot be located"
	}
	if stored == "" {
		return false, "this row records no path"
	}
	root, err := filepath.Abs(c.cfg.LibraryRoot)
	if err != nil {
		return false, "the configured library_root is not usable"
	}
	candidate := stored
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return false, "the recorded path is not usable"
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, "the recorded path is outside the configured library_root"
	}
	fi, err := os.Stat(abs)
	switch {
	case err != nil:
		return false, "the file is not present on disk"
	case !fi.Mode().IsRegular():
		return false, "the recorded path is not a regular file"
	}
	return true, ""
}

// Subscription is one tracked series.
type Subscription struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Subscriptions returns a page of subscriptions.
func (c *Client) Subscriptions(ctx context.Context, limit, offset int) ([]Subscription, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	table := "subscriptions"
	idCol := c.pick(table, "id")
	titleCol := c.pick(table, "title", "name", "query")
	if idCol == "" || titleCol == "" {
		return nil, 0, fmt.Errorf("%w: the subscriptions table has no id and title columns "+
			"this build recognises; columns present: %v", ErrSchema, keys(c.columns[table]))
	}
	var total int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+idCol+", "+titleCol+" FROM "+table+" ORDER BY "+idCol+" LIMIT ? OFFSET ?",
		limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.Title); err != nil {
			return nil, 0, err
		}
		out = append(out, s)
	}
	return out, total, rows.Err()
}

// Task is one download task suzu is running.
type Task struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Progress int64  `json:"progress"`
	Total    int64  `json:"total"`
}

// Tasks returns suzu's download tasks.
//
// These are suzu's tasks, not the console's. They are shown so an operator can
// see that the machine is busy before starting album work; the console never
// starts, stops or modifies one.
func (c *Client) Tasks(ctx context.Context) ([]Task, error) {
	table := "tasks"
	idCol := c.pick(table, "id")
	stateCol := c.pick(table, "state", "status")
	doneCol := c.pick(table, "done_bytes", "progress")
	totalCol := c.pick(table, "total_bytes", "total")
	if idCol == "" {
		return nil, fmt.Errorf("%w: the tasks table has no id column this build "+
			"recognises; columns present: %v", ErrSchema, keys(c.columns[table]))
	}
	selects := []string{idCol}
	for _, optional := range []string{stateCol, doneCol, totalCol} {
		if optional != "" {
			selects = append(selects, optional)
		}
	}
	rows, err := c.db.QueryContext(ctx,
		"SELECT "+strings.Join(selects, ", ")+" FROM "+table+" ORDER BY "+idCol+" DESC LIMIT 100")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var (
			id    string
			state sql.NullString
			done  sql.NullInt64
			total sql.NullInt64
		)
		dest := []any{&id}
		for _, name := range selects[1:] {
			switch name {
			case stateCol:
				dest = append(dest, &state)
			case doneCol:
				dest = append(dest, &done)
			case totalCol:
				dest = append(dest, &total)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, Task{ID: id, State: state.String, Progress: done.Int64, Total: total.Int64})
	}
	return out, rows.Err()
}

// Status is a summary of the integration, suitable for the console's page.
type Status struct {
	Configured bool   `json:"configured"`
	DBPath     string `json:"db_path,omitempty"`
	APIBase    string `json:"api_base,omitempty"`
	Note       string `json:"note"`
	// Tables reports what was found, so an operator can see the schema the
	// console is reading.
	Tables []string `json:"tables,omitempty"`
	// Counts is best-effort: an unreadable count is omitted, not reported as 0.
	LibraryCount      *int `json:"library_count,omitempty"`
	SubscriptionCount *int `json:"subscription_count,omitempty"`
	TaskCount         *int `json:"task_count,omitempty"`
}

// Status reports the integration's state.
func (c *Client) Status(ctx context.Context) Status {
	if c == nil || c.cfg.DBPath == "" {
		return Status{
			Configured: false,
			Note: "the IWantWatchAnime integration is not configured; set hub.suzu.db_path " +
				"to enable it",
		}
	}
	st := Status{
		Configured: true,
		DBPath:     c.cfg.DBPath,
		APIBase:    c.cfg.APIBase,
		Note:       "read-only: the console reads suzu's database and never writes it",
	}
	for _, t := range requiredTables {
		if c.columns[t] != nil {
			st.Tables = append(st.Tables, t)
		}
	}
	if _, n, err := c.Library(ctx, "", 1, 0); err == nil {
		st.LibraryCount = &n
	}
	if _, n, err := c.Subscriptions(ctx, 1, 0); err == nil {
		st.SubscriptionCount = &n
	}
	if ts, err := c.Tasks(ctx); err == nil {
		n := len(ts)
		st.TaskCount = &n
	}
	if c.cfg.LibraryRoot == "" {
		st.Note += "; no library_root is configured, so playback is disabled and the " +
			"console will not guess where episodes are"
	}
	return st
}

// keys returns a map's keys, sorted, for an error message.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion sort: the lists are tiny and this avoids an import.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// escapeLike escapes the LIKE wildcards in a caller-supplied search term, so that
// a query containing % matches a literal percent sign rather than everything.
func escapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
