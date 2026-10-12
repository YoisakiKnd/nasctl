package nasctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubweb"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/suzu"
)

// cmdServe runs the web console in the foreground.
//
// The console is deliberately a foreground process rather than a daemon: under
// systemd it is the unit, and its logs belong to the journal. Running it in the
// foreground also means an operator who starts it by hand can see what it says
// and stop it with Ctrl-C, which is what one wants from a tool being tried on a
// machine that has already had one bad day.
func cmdServe(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	bind := cfg.Hub.Bind
	if bind == "" {
		bind = cfg.Server.Bind
	}
	if bind == "" {
		fmt.Fprintln(env.Stderr, "nasctl serve: no bind address is configured")
		return 2
	}

	// The bind address is this console's access control, so it is checked before
	// the listener exists rather than documented after it does.
	if err := hubweb.CheckBind(bind, cfg.Server.AllowNonPrivateBind); err != nil {
		fmt.Fprintf(env.Stderr, "nasctl serve: %v\n", err)
		return 1
	}

	st, err := openStore(cfg, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer st.Close()

	jm, err := newManager(cfg, st, g)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}

	// Reconcile before serving, so the first page an operator sees does not claim
	// a job is running when its process died with the last console.
	interrupted, orphaned, err := jm.Reconcile(ctx, processAlive)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: could not reconcile job state: %v\n", err)
	}
	if interrupted > 0 {
		fmt.Fprintf(env.Stdout, "reconciled %d job(s) that were open when the console stopped; "+
			"they are recorded as interrupted, not as failures\n", interrupted)
	}
	for _, j := range orphaned {
		fmt.Fprintf(env.Stdout, "warning: job %s (%s) still has a live process (pid %d). The "+
			"console did not start it in this life, so it will not adopt or kill it; "+
			"inspect it before assuming anything about its state\n", j.ID, j.Op, j.PID)
	}

	// Prune on startup rather than on a timer: the console has no scheduler, and
	// this is the only moment it is certain nothing else is writing.
	if n, err := st.PruneHistory(ctx); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "pruned %d finished job record(s) beyond the history cap\n", n)
	}
	if n, err := st.PruneAudit(ctx, env.Now()); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "pruned %d audit record(s) beyond the retention window\n", n)
	}
	if n, err := st.PurgeExpiredModes(ctx, env.Now()); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "cleared %d expired mode(s)\n", n)
	}

	fm, err := files.New(cfg.Hub.Storage, files.DefaultLimits())
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer fm.Close()

	var sc *suzu.Client
	if cfg.Hub.Suzu.DBPath != "" {
		sc, err = suzu.Open(suzu.Config{
			DBPath: cfg.Hub.Suzu.DBPath, APIBase: cfg.Hub.Suzu.APIBase,
			Token: cfg.Hub.Suzu.Token, LibraryRoot: cfg.Hub.Suzu.LibraryRoot,
		})
		if err != nil {
			// A suzu problem must not stop the console: the rest of it is still
			// useful, and the media page reports the reason.
			fmt.Fprintf(env.Stderr, "nasctl: the IWantWatchAnime integration is unavailable: %v\n", err)
			sc = nil
		} else {
			defer sc.Close()
		}
	}

	var logger *logx.Logger
	if l, err := logx.New(logx.Options{Level: "info"}); err == nil {
		logger = l
		defer logger.Close()
	}

	var agents []hubweb.AgentCredential
	for _, a := range cfg.Hub.Agents {
		agents = append(agents, hubweb.AgentCredential{
			Name: a.Name, Token: a.Token, AllowHeavy: a.AllowHeavy,
		})
	}

	srv := hubweb.New(hubweb.Config{
		Bind:                  bind,
		Token:                 cfg.Server.Token,
		Agents:                agents,
		MaxConcurrentRequests: cfg.Server.MaxConcurrentRequests,
		MaxConcurrentStreams:  cfg.Server.MaxConcurrentStreams,
		MaxResponseBytes:      cfg.Server.MaxResponseBytes,
		ReadHeaderTimeout:     cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:           cfg.Server.ReadTimeout.D(),
		WriteTimeout:          cfg.Server.WriteTimeout.D(),
		IdleTimeout:           cfg.Server.IdleTimeout.D(),
		PageDefault:           cfg.Server.PageDefault,
		PageMax:               cfg.Server.PageMax,
		PreviewMaxBytes:       32 << 20,
	}, fm, jm, sc, logger.Logger)

	httpSrv := &http.Server{
		Addr:              bind,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
	}

	fmt.Fprintf(env.Stdout, "nasctl %s listening on %s\n", buildinfo.Version, bind)
	fmt.Fprintf(env.Stdout, "  read-only console: it does not scan, decode, transcode or write files\n")
	if cfg.Server.Token == "" {
		fmt.Fprintf(env.Stdout, "  no token is configured; the bind address is the access control\n")
	}
	for _, a := range cfg.Hub.Agents {
		heavy := "may not start heavy work"
		if a.AllowHeavy {
			heavy = "MAY start bounded heavy work"
		}
		fmt.Fprintf(env.Stdout, "  agent %q is configured with its own credential and %s\n",
			a.Name, heavy)
	}
	if len(cfg.Hub.Storage) == 0 {
		fmt.Fprintf(env.Stdout, "  no storage roots are configured, so the file manager is empty\n")
	}
	if sc == nil && cfg.Hub.Suzu.DBPath != "" {
		fmt.Fprintf(env.Stdout, "  the watch integration is unavailable; see the message above\n")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	case <-ctx.Done():
		fmt.Fprintln(env.Stdout, "shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(),
		cfg.Server.ShutdownTimeout.D())
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: shutdown did not complete cleanly: %v\n", err)
		return 1
	}
	return 0
}

// processAlive reports whether a pid is still running and belongs to something
// this console could have started.
//
// It deliberately checks only liveness, not ownership: /proc gives no reliable
// way to prove a process came from this application after a restart, and a wrong
// "yes" here means the console reports an orphan for a human to look at, while a
// wrong "no" would let it mark a live job interrupted. Reporting for a human is
// the safer error, so the check is conservative.
func processAlive(pid, pgid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); err != nil {
		return false
	}
	return true
}

// waitForHealth waits until the console answers, for use by tests and by an
// operator script that starts it in the background.
func waitForHealth(ctx context.Context, addr string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/v1/overview", nil)
		if err != nil {
			return err
		}
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("the console did not become ready at %s", addr)
}
