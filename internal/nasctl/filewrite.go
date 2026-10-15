package nasctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func cmdFileWrite(ctx context.Context, env Env, cfg *config.Config, g globals, fm *files.Manager, op string, argv []string) int {
	fs := flag.NewFlagSet("files "+op, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	root := fs.String("root", "", "storage ID")
	p := fs.String("path", "", "root-relative path")
	from := fs.String("from", "", "source root-relative path")
	to := fs.String("to", "", "destination root-relative path")
	toRoot := fs.String("to-root", "", "destination storage ID")
	id := fs.String("id", "", "trash ID")
	local := fs.String("source", "", "local regular file to upload")
	batchFile := fs.String("batch-file", "", "bounded batch JSON file (128 KiB maximum)")
	confirm := fs.Bool("confirm", false, "confirm this operator mutation")
	confirmName := fs.String("confirm-name", "", "exact trash name for irreversible purge")
	limit := fs.Int("limit", fm.Limits().CopyMaxEntries, "shared maximum entries")
	maxBytes := fs.Int64("max-bytes", fm.Limits().CopyMaxBytes, "shared maximum copy bytes")
	if err := fs.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || !*confirm {
		fmt.Fprintln(env.Stderr, "nasctl files: explicit --confirm and named arguments are required")
		return 2
	}
	if g.asAgent != "" {
		fmt.Fprintln(env.Stderr, "nasctl files: writes require a local operator, not --agent")
		return 1
	}
	args := operations.NewArgs().Str("root", *root)
	var action func(context.Context, func(int64, int64, string)) (any, error)
	switch op {
	case "mkdir":
		args.Str("path", *p)
		action = func(c context.Context, _ func(int64, int64, string)) (any, error) { return nil, fm.Mkdir(c, *root, *p) }
	case "rename":
		args.Str("from", *from).Str("to", *to)
		action = func(c context.Context, _ func(int64, int64, string)) (any, error) {
			return nil, fm.Rename(c, *root, *from, *to)
		}
	case "trash":
		args.Str("path", *p)
		action = func(c context.Context, _ func(int64, int64, string)) (any, error) { return fm.Trash(c, *root, *p) }
	case "restore":
		args.Str("id", *id)
		if *to != "" {
			args.Str("to", *to)
		}
		action = func(c context.Context, _ func(int64, int64, string)) (any, error) {
			return fm.Restore(c, *root, *id, *to)
		}
	case "purge":
		args.Str("id", *id).Str("confirm_name", *confirmName).Int("limit", int64(*limit))
		action = func(c context.Context, report func(int64, int64, string)) (any, error) {
			return fm.Purge(c, *root, *id, *confirmName, *limit, func(n, t int64) { report(n, t, "entries") })
		}
	case "copy", "move":
		o, err := files.ValidateCopy(files.CopyOptions{SourceRoot: *root, DestinationRoot: *toRoot, From: *from, To: *to, Limit: *limit, MaxBytes: *maxBytes, Move: op == "move"})
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 2
		}
		args.Str("to_root", *toRoot).Str("from", o.From).Str("to", o.To).Int("limit", int64(*limit)).Int("max_bytes", *maxBytes)
		action = func(c context.Context, report func(int64, int64, string)) (any, error) {
			return fm.Copy(c, o, func(_, b int64, _ string) { report(b, 0, "bytes") })
		}
	case "upload":
		f, err := os.OpenFile(*local, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 2
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			fmt.Fprintln(env.Stderr, "upload source must be a regular file")
			return 2
		}
		args.Str("path", *p).Int("size", info.Size())
		action = func(c context.Context, _ func(int64, int64, string)) (any, error) {
			return fm.Upload(c, *root, *p, f, info.Size())
		}
	case "batch":
		f, err := os.OpenFile(*batchFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 2
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 {
			fmt.Fprintln(env.Stderr, "batch file must be regular and no larger than 128 KiB")
			return 2
		}
		var input files.BatchOptions
		d := json.NewDecoder(io.LimitReader(f, (128<<10)+1))
		d.DisallowUnknownFields()
		if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
			fmt.Fprintln(env.Stderr, "invalid batch JSON")
			return 2
		}
		o, err := fm.ValidateBatch(input)
		if err != nil {
			fmt.Fprintln(env.Stderr, err)
			return 2
		}
		args = operations.NewArgs().Str("action", o.Action).Str("selection", o.Digest()).Int("count", int64(len(o.Items))).Int("limit", int64(o.Limit)).Int("max_bytes", o.MaxBytes)
		action = func(c context.Context, report func(int64, int64, string)) (any, error) {
			return fm.Batch(c, o, func(p files.BatchResult) { report(int64(p.Completed+p.Failed), int64(len(o.Items)), "items") })
		}
	}
	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	defer st.Close()
	jm, err := newManager(cfg, st, g)
	if err != nil {
		fmt.Fprintln(env.Stderr, err)
		return 1
	}
	declaration, ok := jm.Registry().Lookup("files." + op)
	if !ok || declaration.Check(args) != nil {
		fmt.Fprintln(env.Stderr, "invalid file operation arguments")
		return 2
	}
	work, stop := context.WithTimeout(ctx, declaration.Timeout)
	defer stop()
	target := *root
	if op == "batch" {
		target = args.String("selection")
	}
	var result any
	out := jm.Run(work, jobs.RunOptions{Principal: principalFor(g), OpID: declaration.ID, Args: args, Target: target, OperatorConsent: true, Handler: func(c context.Context, j *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		last := time.Time{}
		result, err = action(c, func(n, total int64, unit string) {
			if time.Since(last) >= 500*time.Millisecond {
				last = time.Now()
				_ = st.Progress(c, j.ID, 0, 0, n, total, unit)
			}
		})
		finish, cancel := context.WithTimeout(context.WithoutCancel(c), 5*time.Second)
		defer cancel()
		var amount, total int64
		unit := "entries"
		switch value := result.(type) {
		case files.CopyResult:
			amount = value.Entries
			if value.Bytes > 0 {
				amount = value.Bytes
				unit = "bytes"
			}
			if err == nil {
				total = amount
			}
		case files.PurgeResult:
			amount = value.Removed
			total = value.Total
		case files.BatchResult:
			amount = int64(value.Completed + value.Failed)
			total = int64(len(value.Items))
			unit = "items"
		case int64:
			amount = value
			unit = "bytes"
			if err == nil {
				total = amount
			}
		default:
			if err == nil {
				amount = 1
				total = 1
			}
		}
		if saveErr := st.Progress(finish, j.ID, 0, 0, amount, total, unit); saveErr != nil {
			return jobs.Result{}, saveErr
		}
		note, e := json.Marshal(map[string]any{"kind": "files." + op, "result": result})
		if e == nil {
			e = st.Transition(finish, j.ID, domain.JobRunning, string(note))
		}
		if e != nil {
			return jobs.Result{}, e
		}
		return jobs.Result{Completed: err == nil, Verified: err == nil}, err
	}})
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = st.PruneHistory(finish)
	_, _ = st.PruneAudit(finish, time.Now())
	doc := map[string]any{"job": out.Job, "result": result}
	if out.Err != nil {
		doc["error"] = files.BatchErrorCode(out.Err)
	}
	emit(env.Stdout, g.jsonOut, doc, func(w io.Writer) { _ = json.NewEncoder(w).Encode(doc) })
	if out.Err != nil {
		fmt.Fprintf(env.Stderr, "nasctl files: %v\n", out.Err)
		return 1
	}
	return 0
}
