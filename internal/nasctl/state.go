package nasctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/statebackup"
)

func cmdState(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, "nasctl state: use backup, verify, restore or recover (-h for arguments)")
		return 2
	}
	fs := flag.NewFlagSet("state "+args[0], flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	p := fs.String("package", "", "absolute private package directory (new for backup)")
	digest := fs.String("manifest-sha256", "", "exact SHA-256 returned by state verify, required for restore")
	confirm := fs.Bool("confirm", false, "confirm offline local operator operation")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	if g.asAgent != "" {
		fmt.Fprintln(env.Stderr, "nasctl state: local operator required")
		return 1
	}
	if args[0] != "verify" && !*confirm {
		fmt.Fprintln(env.Stderr, "nasctl state: explicit --confirm required; stop nasctl and albumctl writers first")
		return 2
	}
	configPath := g.configPath
	if configPath == "" {
		configPath = "/etc/albumctl/config.toml"
	}
	configPath, _ = filepath.Abs(configPath)
	l := statebackup.NewLayout(cfg, configPath, g.hubDB)
	ctx, stop := context.WithTimeout(ctx, 30*time.Minute)
	defer stop()
	report := func(phase string) { fmt.Fprintln(env.Stderr, phase) }
	var result statebackup.Result
	var err error
	switch args[0] {
	case "backup":
		result, err = statebackup.Create(ctx, l, *p, report)
	case "verify":
		result, err = statebackup.Verify(ctx, *p)
	case "restore":
		result, err = statebackup.Restore(ctx, l, *p, *digest, report)
	case "recover":
		err = statebackup.Recover(ctx, l)
		if err == nil {
			fmt.Fprintln(env.Stdout, "Offline state recovery completed; writers may now be restarted.")
			return 0
		}
	default:
		fmt.Fprintln(env.Stderr, "nasctl state: unknown action")
		return 2
	}
	if err != nil {
		fmt.Fprintf(env.Stderr, "nasctl state: %v\n", err)
		if result.Checkpoint != "" {
			fmt.Fprintln(env.Stderr, "Preserved checkpoint:", result.Checkpoint)
		}
		return 1
	}
	emit(env.Stdout, g.jsonOut, result, func(w io.Writer) {
		fmt.Fprintln(w, result.String())
		if result.Checkpoint != "" {
			fmt.Fprintln(w, "Preserved checkpoint:", result.Checkpoint)
			fmt.Fprintln(w, "Configuration was saved in the package; the active configuration was not replaced.")
		}
	})
	return 0
}
