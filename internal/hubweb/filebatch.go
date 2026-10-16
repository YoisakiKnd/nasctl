package hubweb

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func (s *Server) handleFileBatch(w http.ResponseWriter, r *http.Request) {
	if !s.fileOperator(w, r, true) {
		return
	}
	if r.Header.Get("X-Nasctl-Confirm") != "1" {
		writeError(w, 400, "confirmation_required", "请核对并确认整批文件操作")
		return
	}
	var input files.BatchOptions
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil {
		writeError(w, 400, "bad_request", "批量文件操作 JSON 无效")
		return
	}
	if d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "bad_request", "必须提交单个 JSON 对象")
		return
	}
	o, err := s.Files.ValidateBatch(input)
	if err != nil {
		s.fileWriteError(w, err)
		return
	}
	gate := s.privateGate(r)
	use := map[string]privateUse{"copy": useCopy, "move": useMove, "trash": useMove}[o.Action]
	var refused error
	if o.Action == "copy" || o.Action == "move" {
		refused = gate.check(o.ToRoot, o.Directory, useInside)
	}
	for _, item := range o.Items {
		if refused != nil {
			break
		}
		if o.Action == "restore" {
			refused = gate.trashID(item.Root, item.ID)
		} else {
			refused = gate.check(item.Root, item.Path, use)
		}
	}
	if privateError(w, refused) {
		return
	}
	s.startBackground(w, r, "files.batch", FileBatchArgs(o), o.Digest(), true, func(ctx context.Context, job *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		last := time.Time{}
		result, err := s.Files.Batch(ctx, o, func(p files.BatchResult) {
			if time.Since(last) < 500*time.Millisecond {
				return
			}
			last = time.Now()
			_ = s.Jobs.Store().Progress(ctx, job.ID, 0, 0, int64(p.Completed+p.Failed), int64(len(o.Items)), "items")
		})
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_ = s.Jobs.Store().Progress(finish, job.ID, 0, 0, int64(result.Completed+result.Failed), int64(len(o.Items)), "items")
		note, _ := json.Marshal(map[string]any{"kind": "files.batch", "result": result})
		if saveErr := s.Jobs.Store().Transition(finish, job.ID, domain.JobRunning, string(note)); saveErr != nil {
			return jobs.Result{}, saveErr
		}
		return jobs.Result{Completed: err == nil, Verified: err == nil}, err
	})
}

func FileBatchArgs(o files.BatchOptions) *operations.Args {
	return operations.NewArgs().Str("action", o.Action).Str("selection", o.Digest()).Int("count", int64(len(o.Items))).Int("limit", int64(o.Limit)).Int("max_bytes", o.MaxBytes)
}
