package nasctl

import (
	"context"
	"errors"
	"fmt"
	"github.com/YoisakiKnd/nasctl/internal/storage"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/albumlocal"
	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/backuplocal"
	"github.com/YoisakiKnd/nasctl/internal/buildinfo"
	"github.com/YoisakiKnd/nasctl/internal/cloudbackup"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/hubweb"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/logx"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
	"github.com/YoisakiKnd/nasctl/internal/resource"
	"github.com/YoisakiKnd/nasctl/internal/settingslocal"
	"github.com/YoisakiKnd/nasctl/internal/statelease"
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
	serviceLease, err := statelease.AcquireService(cfg.Paths.StateDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "nasctl serve:", err)
		return 1
	}
	defer serviceLease.Close()
	users, err := accountslocal.Open(cfg.Paths.StateDir)
	if err != nil {
		fmt.Fprintln(env.Stderr, "nasctl: account state could not be opened; inspect or restore it locally:", err)
		return 1
	}
	defer users.Close()
	if err = users.EnsureSetupKey(ctx); err != nil {
		fmt.Fprintln(env.Stderr, "nasctl: first-account setup state could not be prepared:", err)
		return 1
	}
	accountState, err := users.Snapshot(ctx)
	if err != nil {
		fmt.Fprintln(env.Stderr, "nasctl:", err)
		return 1
	}
	if accountState.Enabled && !cfg.Hub.Setup && cfg.Server.Token == "" {
		fmt.Fprintln(env.Stderr, "nasctl: enabled accounts require [hub] setup=true or server.token in the base configuration; refusing anonymous fallback if the state volume is lost")
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
	animeEngine, err := anime.Open(cfg.Hub.Anime, cfg.Paths.StateDir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	workCtx, stopWork := context.WithCancel(ctx)
	var background sync.WaitGroup
	defer func() {
		stopWork()
		background.Wait()
		if animeEngine != nil {
			animeEngine.Close()
		}
	}()
	if animeEngine != nil {
		animeEngine.Admit = func(ctx context.Context) error {
			mode, err := jm.EffectiveMode(ctx)
			if err != nil {
				return err
			}
			if !mode.AllowsHeavyWork() {
				return fmt.Errorf("anime work is paused by mode %s", mode.Mode)
			}
			if runtime.GOOS == "linux" && jm.Guard() != nil {
				d := jm.Guard().BeforeFile()
				if d.Verdict != resource.Allow {
					return fmt.Errorf("anime resource guard: %s", d.Detail)
				}
			}
			return nil
		}
		runAnime := func(ctx context.Context, op string, action func(context.Context, int64) (anime.RefreshResult, error)) {
			jm.Run(ctx, jobs.RunOptions{Principal: domain.Principal{Kind: domain.PrincipalLocal, Name: "scheduler"}, OpID: op, Args: operations.NewArgs().Int("subscription", 0), Target: "all", Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
				result, err := action(ctx, 0)
				if err == nil && len(result.Errors) > 0 {
					err = fmt.Errorf("%s", result.Errors[0])
				}
				return jobs.Result{Completed: err == nil, Detail: fmt.Sprintf("discovered %d, submitted %d, archived %d", result.Discovered, result.Submitted, result.Archived)}, err
			}})
			// Scheduled work must not rely on a process restart to enforce the
			// configured history and audit retention limits.
			pruneCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if _, err := st.PruneHistory(pruneCtx); err != nil {
				fmt.Fprintf(env.Stderr, "nasctl: history retention: %v\n", err)
			}
			if _, err := st.PruneAudit(pruneCtx, env.Now()); err != nil {
				fmt.Fprintf(env.Stderr, "nasctl: audit retention: %v\n", err)
			}
		}
		animeEngine.OnPoll = func(ctx context.Context) { runAnime(ctx, "anime.refresh", animeEngine.Refresh) }
		animeEngine.OnSync = func(ctx context.Context) {
			pending, err := animeEngine.Pending(ctx)
			if err != nil {
				fmt.Fprintf(env.Stderr, "nasctl: pending anime work: %v\n", err)
				return
			}
			if pending {
				runAnime(ctx, "anime.sync", animeEngine.Sync)
			}
		}
		background.Add(1)
		go func() { defer background.Done(); animeEngine.Run(workCtx) }()
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

	// Prune history on startup; the anime scheduler uses its own database.
	if n, err := st.PruneHistory(ctx); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "pruned %d finished job record(s) beyond the history cap\n", n)
	}
	if n, err := st.PruneAudit(ctx, env.Now()); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "pruned %d audit record(s) beyond the retention window\n", n)
	}
	if n, err := st.PurgeExpiredModes(ctx, env.Now()); err == nil && n > 0 {
		fmt.Fprintf(env.Stdout, "cleared %d expired mode(s)\n", n)
	}

	fm, err := files.New(cfg.Hub.Storage, files.ConfigLimits(cfg.Hub.Files))
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: %v\n", err)
		return 1
	}
	defer fm.Close()
	backupControl, backupErr := backuplocal.New(cfg.Paths.StateDir, fm)
	if backupErr != nil {
		fmt.Fprintf(env.Stderr, "nasctl: local backup configuration could not be loaded: %v\n", backupErr)
	} else {
		defer backupControl.Close()
	}
	var albumClient *album.Client
	var albumControl *albumlocal.Control
	switch cfg.Hub.Album.EffectiveMode() {
	case "local":
		albumControl, err = albumlocal.NewControl(cfg, fm, nil)
		if err == nil {
			defer albumControl.Close()
			albumClient, err = album.NewHandler(albumControl.Reader)
		}
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl: album setup: %v\n", err)
		return 1
	}
	defer albumClient.Close()

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

	baseLog := slog.Default()
	if logger != nil {
		baseLog = logger.Logger
	}
	srv := hubweb.New(hubweb.Config{
		Bind:                  bind,
		RequireSetup:          cfg.Hub.Setup,
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
	}, fm, jm, sc, baseLog)
	srv.Anime = animeEngine
	srv.Accounts = users
	srv.Settings = settingslocal.New(cfg)
	desktop := desktopapps.New(cfg.Paths.StateDir)
	srv.Apps = desktop
	srv.Wallpaper = desktop
	srv.Storage = storage.Client{}
	if err := srv.EnableRememberedSessions(cfg.Paths.StateDir); err != nil {
		// Kept sessions are a convenience: an unreadable file signs members out
		// rather than stopping the console.
		fmt.Fprintf(env.Stderr, "nasctl: remembered sessions ignored: %v\n", err)
	}
	cloudBackups, cloudErr := cloudbackup.New(cfg.Paths.StateDir, fm)
	if cloudErr != nil {
		fmt.Fprintf(env.Stderr, "nasctl: WebDAV backup configuration could not be loaded: %v\n", cloudErr)
	} else {
		defer cloudBackups.Close()
		srv.CloudBackups = cloudBackups
	}
	srv.Album = albumClient
	if albumControl != nil {
		srv.AlbumControl = albumControl
		vault, err := photovault.Open(cfg.Paths.StateDir)
		if err != nil {
			fmt.Fprintf(env.Stderr, "nasctl: cannot open photo vault: %v\n", err)
			return 1
		}
		defer vault.Close()
		srv.PhotoVault = vault
		if cloudBackups != nil {
			cloudBackups.Vault = vault
		}
		srv.VaultMover = albumControl
	}
	srv.WorkContext = workCtx
	if backupControl != nil {
		srv.Backups = backupControl
	}
	defer func() { stopWork(); srv.WaitWork() }()

	httpSrv := &http.Server{
		Addr:              bind,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.D(),
		ReadTimeout:       cfg.Server.ReadTimeout.D(),
		WriteTimeout:      cfg.Server.WriteTimeout.D(),
		IdleTimeout:       cfg.Server.IdleTimeout.D(),
	}

	fmt.Fprintf(env.Stdout, "nasctl %s listening on %s\n", buildinfo.Version, bind)
	fmt.Fprintf(env.Stdout, "  NAS console: native anime subscriptions and configured storage roots\n")
	if cfg.Server.Token == "" {
		if accountState.Enabled {
			fmt.Fprintln(env.Stdout, "  local account login is required")
		} else if cfg.Hub.Setup {
			fmt.Fprintln(env.Stdout, "  first administrator setup is required; read the private setup key on this device")
		} else {
			fmt.Fprintln(env.Stdout, "  legacy local operator mode; configure an account or server token to require login")
		}
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
	stopWork()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		_ = httpSrv.Close()
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
