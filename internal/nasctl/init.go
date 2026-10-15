package nasctl

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/YoisakiKnd/nasctl/internal/accountslocal"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/hubweb"
)

func cmdInit(ctx context.Context, env Env, g globals, args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	state := fs.String("state-dir", "", "absolute system-state directory")
	cache := fs.String("cache-dir", "", "absolute cache directory (default state/cache)")
	bind := fs.String("bind", "127.0.0.1:8090", "private or loopback NAS bind address")
	confirm := fs.Bool("confirm", false, "create a new NAS configuration and private setup challenge")
	if e := fs.Parse(args); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || !*confirm || g.configPath == "" || g.asAgent != "" || !filepath.IsAbs(*state) || filepath.Clean(*state) == "/" {
		fmt.Fprintln(env.Stderr, "nasctl init: local operator, explicit --config, absolute --state-dir and --confirm required")
		return 2
	}
	dest, e := filepath.Abs(g.configPath)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	if _, e = os.Lstat(dest); e == nil || !os.IsNotExist(e) {
		fmt.Fprintln(env.Stderr, "nasctl init: existing configuration is never replaced")
		return 1
	}
	cfg := config.Default()
	cfg.Paths.StateDir = *state
	cfg.Paths.CacheDir = *cache
	cfg.Server.Bind = *bind
	cfg.Hub.Bind = *bind
	cfg.Hub.Setup = true
	if e = cfg.Finalize(); e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	if e = hubweb.CheckBind(*bind, false); e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	for _, dir := range []string{cfg.Paths.StateDir, cfg.Paths.CacheDir, filepath.Dir(dest)} {
		if e = os.MkdirAll(dir, 0750); e != nil {
			fmt.Fprintln(env.Stderr, e)
			return 1
		}
	}
	users, e := accountslocal.Open(cfg.Paths.StateDir)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	defer users.Close()
	if e = users.EnsureSetupKey(ctx); e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	accountState, e := users.Snapshot(ctx)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	var b bytes.Buffer
	if e = toml.NewEncoder(&b).Encode(cfg); e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	root, e := os.OpenRoot(filepath.Dir(dest))
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	defer root.Close()
	var token [16]byte
	if _, e = rand.Read(token[:]); e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	tmp := ".nasctl-init-" + hex.EncodeToString(token[:]) + ".tmp"
	f, e := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	defer root.Remove(tmp)
	_, e = f.Write(b.Bytes())
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e == nil {
		e = ctx.Err()
	}
	if e == nil {
		e = root.Link(tmp, filepath.Base(dest))
	}
	if e != nil {
		fmt.Fprintln(env.Stderr, "nasctl init:", e)
		return 1
	}
	dir, e := root.Open(".")
	if e != nil {
		fmt.Fprintln(env.Stderr, e)
		return 1
	}
	e = dir.Sync()
	dir.Close()
	if e != nil {
		fmt.Fprintln(env.Stderr, "nasctl init: config created but directory sync failed:", e)
		return 1
	}
	out := map[string]any{"config": dest, "state_dir": cfg.Paths.StateDir, "setup_required": !accountState.Enabled}
	emit(env.Stdout, g.jsonOut, out, func(w io.Writer) {
		fmt.Fprintln(w, "Created NAS configuration:", dest)
		if accountState.Enabled {
			fmt.Fprintln(w, "Existing local accounts will be used.")
		} else {
			fmt.Fprintln(w, "First administrator setup is required. Read the private challenge using nasctl users setup-code with this configuration, then open the NAS setup page.")
		}
	})
	return 0
}
