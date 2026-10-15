package nasctl

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
)

func readAccountPassword(ctx context.Context, input io.Reader) (string, error) {
	ctx, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	type result struct {
		b []byte
		e error
	}
	done := make(chan result, 1)
	go func() { b, e := io.ReadAll(io.LimitReader(input, 1027)); done <- result{b, e} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-done:
		if out.e != nil {
			return "", out.e
		}
		if len(out.b) > 1026 {
			return "", accounts.ErrInvalid
		}
		password := strings.TrimSuffix(string(out.b), "\n")
		password = strings.TrimSuffix(password, "\r")
		if len(password) > 1024 || strings.ContainsAny(password, "\x00\r\n") {
			return "", accounts.ErrInvalid
		}
		return password, nil
	}
}
func cmdUsers(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "nasctl users: use list, setup-code, create, set, password, remove or recover (-h for flags)")
		return 2
	}
	fs := flag.NewFlagSet("users "+args[0], flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	username := fs.String("username", "", "immutable login name")
	id := fs.String("id", "", "account ID (optional when username is supplied)")
	display := fs.String("display-name", "", "display name")
	role := fs.String("role", "", "admin, operator or viewer")
	disabled := fs.Bool("disabled", false, "disable account")
	passwordStdin := fs.Bool("password-stdin", false, "read new password from stdin, not arguments")
	confirm := fs.Bool("confirm", false, "confirm this local account mutation")
	confirmUsername := fs.String("confirm-username", "", "exact username required for removal")
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || g.asAgent != "" {
		fmt.Fprintln(env.Stderr, "nasctl users: named arguments and a local OS operator are required")
		return 2
	}
	if args[0] == "recover" {
		if !*confirm || !*passwordStdin || *username == "" || *id != "" || *role != "" || *display != "" || *disabled || *confirmUsername != "" {
			fmt.Fprintln(env.Stderr, "nasctl users recover: --username, --password-stdin and --confirm required; this replaces all account records and revokes their sessions, preserving the original in private quarantine")
			return 2
		}
		if !cfg.Hub.Setup && cfg.Server.Token == "" {
			fmt.Fprintln(env.Stderr, "nasctl users recover: first enable [hub] setup=true or server.token in the base configuration")
			return 2
		}
		password, e := readAccountPassword(ctx, env.Stdin)
		if e != nil {
			fmt.Fprintln(env.Stderr, "nasctl users recover: invalid password input")
			return 2
		}
		out, e := accountslocal.Recover(ctx, cfg.Paths.StateDir, *username, password)
		if out.Quarantine != "" {
			emit(env.Stdout, g.jsonOut, out, func(w io.Writer) { fmt.Fprintln(w, "Private recovery record:", out.Quarantine) })
		}
		if e != nil {
			fmt.Fprintln(env.Stderr, "nasctl users recover:", e)
			return 1
		}
		return 0
	}
	c, e := accountslocal.Open(cfg.Paths.StateDir)
	if e != nil {
		fmt.Fprintln(env.Stderr, "nasctl users:", e)
		return 1
	}
	defer c.Close()
	if args[0] == "setup-code" {
		if e = c.EnsureSetupKey(ctx); e != nil {
			fmt.Fprintln(env.Stderr, e)
			return 1
		}
		key, e := c.SetupKey(ctx)
		if e != nil {
			fmt.Fprintln(env.Stderr, e)
			return 1
		}
		fmt.Fprintln(env.Stdout, key)
		return 0
	}
	snap, e := c.Snapshot(ctx)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	if args[0] == "list" {
		emit(env.Stdout, g.jsonOut, snap, func(w io.Writer) {
			for _, u := range snap.Users {
				fmt.Fprintf(w, "%s  %s  %s  disabled=%t\n", u.ID, u.Username, u.Role, u.Disabled)
			}
			if !snap.Enabled {
				fmt.Fprintln(w, "No local account yet; use the private setup challenge on the NAS setup page.")
			}
		})
		return 0
	}
	if !*confirm {
		fmt.Fprintln(env.Stderr, "nasctl users: explicit --confirm required")
		return 2
	}
	if !snap.Enabled && !cfg.Hub.Setup && cfg.Server.Token == "" {
		fmt.Fprintln(env.Stderr, "nasctl users: first enable [hub] setup=true (or server.token) in the base configuration and restart; login protection must survive loss of the state volume")
		return 2
	}
	var password string
	if *passwordStdin {
		password, e = readAccountPassword(ctx, env.Stdin)
		if e != nil {
			fmt.Fprintln(env.Stderr, "nasctl users: could not read a bounded single-line password:", e)
			return 2
		}
	}
	req := accounts.Request{Revision: snap.Revision, ID: *id, Username: *username, DisplayName: *display, Role: *role, Disabled: *disabled, Password: password}
	var previous *accounts.User
	for _, u := range snap.Users {
		if (*id != "" && u.ID == *id) || (*id == "" && u.Username == *username) {
			copy := u
			previous = &copy
			break
		}
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })
	action := args[0]
	switch action {
	case "create":
		if *username == "" || !*passwordStdin || *id != "" || previous != nil {
			fmt.Fprintln(env.Stderr, "nasctl users create: new username and --password-stdin required")
			return 2
		}
		if req.Role == "" {
			req.Role = "viewer"
		}
		if !snap.Enabled && req.Role != "admin" {
			fmt.Fprintln(env.Stderr, "nasctl users: first account must explicitly use --role admin")
			return 2
		}
	case "set", "password", "remove":
		if previous == nil {
			fmt.Fprintln(env.Stderr, "nasctl users: unknown account")
			return 2
		}
		req.ID = previous.ID
		req.Username = previous.Username
		if !setFlags["display-name"] {
			req.DisplayName = previous.DisplayName
		}
		if !setFlags["role"] {
			req.Role = previous.Role
		}
		if !setFlags["disabled"] {
			req.Disabled = previous.Disabled
		}
		if action == "password" {
			if !*passwordStdin || setFlags["role"] || setFlags["disabled"] || setFlags["display-name"] {
				fmt.Fprintln(env.Stderr, "nasctl users password: only account selection and --password-stdin are accepted")
				return 2
			}
		}
		if action == "remove" && *confirmUsername != previous.Username {
			fmt.Fprintln(env.Stderr, "nasctl users remove: exact --confirm-username required")
			return 2
		}
	default:
		fmt.Fprintln(env.Stderr, "nasctl users: unknown action")
		return 2
	}
	store, e := openStore(cfg, g)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	defer store.Close()
	manager, e := newManager(cfg, store, g)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	revision := req.Revision
	if !snap.Enabled {
		revision = "first-setup"
	}
	params := operations.NewArgs().Str("user", req.Username).Str("revision", revision).Str("action", action).Str("role", req.Role)
	out := manager.Run(ctx, jobs.RunOptions{Principal: principalFor(g), OpID: "users.configure", Args: params, Target: req.Username, Handler: func(ctx context.Context, _ *hubstore.Job, _ *operations.Args) (jobs.Result, error) {
		var e error
		if !snap.Enabled {
			if action != "create" {
				return jobs.Result{}, accounts.ErrConflict
			}
			if e = c.EnsureSetupKey(ctx); e == nil {
				var key string
				key, e = c.SetupKey(ctx)
				if e == nil {
					_, e = c.CreateFirst(ctx, accounts.Setup{Code: key, Username: req.Username, DisplayName: req.DisplayName, Password: req.Password})
				}
			}
		} else if action == "remove" {
			e = c.Remove(ctx, req.ID, req.Revision)
		} else {
			_, e = c.Save(ctx, req)
		}
		return jobs.Result{Completed: e == nil, Verified: e == nil}, e
	}})
	if out.Err != nil {
		fmt.Fprintln(env.Stderr, "nasctl users:", out.Err)
		return 1
	}
	saved, e := c.Snapshot(ctx)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	emit(env.Stdout, g.jsonOut, saved, func(w io.Writer) { b, _ := json.Marshal(saved); fmt.Fprintln(w, string(b)) })
	return 0
}
