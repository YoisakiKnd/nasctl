package nasctl

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/localrepair"
)

func cmdSettings(ctx context.Context, env Env, cfg *config.Config, g globals, args []string) int {
	if len(args) == 0 || args[0] != "reset" {
		fmt.Fprintln(env.Stderr, "nasctl settings: use reset --confirm after stopping the service; the saved override is preserved privately and the base TOML is unchanged")
		return 2
	}
	fs := flag.NewFlagSet("settings reset", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	confirm := fs.Bool("confirm", false, "quarantine saved settings and use base configuration on next startup")
	if e := fs.Parse(args[1:]); e != nil {
		if errors.Is(e, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if !*confirm || fs.NArg() != 0 || g.asAgent != "" {
		fmt.Fprintln(env.Stderr, "nasctl settings reset: local OS operator and explicit --confirm required")
		return 2
	}
	out, e := localrepair.Run(ctx, cfg.Paths.StateDir, "settings.reset", func(root *os.Root, rel string) error { return localrepair.Move(root, config.NASSettingsFile, rel) })
	if out.Quarantine != "" {
		emit(env.Stdout, g.jsonOut, out, func(w io.Writer) { fmt.Fprintln(w, "Private recovery record:", out.Quarantine) })
	}
	if e != nil {
		fmt.Fprintln(env.Stderr, "nasctl settings reset:", e)
		return 1
	}
	return 0
}
