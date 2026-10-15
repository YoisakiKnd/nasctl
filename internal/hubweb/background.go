package hubweb

import (
	"context"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

// startBackground binds a native task to the current process lifetime. Admission,
// durable bookkeeping and cancellation use the same manager as the CLI.
func (s *Server) startBackground(w http.ResponseWriter, r *http.Request, op string, args *operations.Args, target string, consent bool, handler jobs.Handler) {
	declared, ok := s.Jobs.Registry().Lookup(op)
	if !ok || declared.Check(args) != nil {
		writeError(w, 400, "bad_request", "任务参数无效")
		return
	}
	if !s.beginWork() {
		writeError(w, 503, "shutting_down", "服务正在停止")
		return
	}
	base := s.WorkContext
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, declared.Timeout)
	accepted := make(chan string, 1)
	done := make(chan jobs.Outcome, 1)
	principal := principalOf(r.Context())
	go func() {
		defer s.work.Done()
		defer cancel()
		id := ""
		defer func() { s.unregisterActive(id) }()
		out := s.Jobs.Run(ctx, jobs.RunOptions{Principal: principal, OpID: op, Args: args, Target: target, OperatorConsent: consent, Handler: handler, OnAccepted: func(job *hubstore.Job) { id = job.ID; s.registerActive(id, cancel); accepted <- id }})
		done <- out
		pruneCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		_, _ = s.Jobs.Store().PruneHistory(pruneCtx)
		_, _ = s.Jobs.Store().PruneAudit(pruneCtx, time.Now())
	}()
	select {
	case id := <-accepted:
		writeJSON(w, 202, map[string]string{"job_id": id})
	case out := <-done:
		if out.Job != nil {
			writeJSON(w, 202, map[string]string{"job_id": out.Job.ID})
		} else {
			s.fileWriteError(w, out.Err)
		}
	case <-r.Context().Done():
		return
	}
}
