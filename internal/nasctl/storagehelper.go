package nasctl

import (
	"context"
	"fmt"

	"github.com/YoisakiKnd/nasctl/internal/storage"
	"github.com/YoisakiKnd/nasctl/internal/storagehelper"
)

// cmdStorageHelper serves one request from the helper socket on stdin and
// stdout, or, as `storage-helper automount DEVICE`, mounts a disk that udev
// just announced when automatic mounting is on.
func cmdStorageHelper(ctx context.Context, env Env, g globals, args []string) int {
	h := storagehelper.New(func() []string {
		cfg, err := loadConfig(g)
		if err != nil {
			return nil
		}
		bases := make([]string, 0, len(cfg.Hub.Storage))
		for _, s := range cfg.Hub.Storage {
			bases = append(bases, s.Path)
		}
		return bases
	})
	switch {
	case len(args) == 0:
		h.Serve(ctx, env.Stdin, env.Stdout)
		return 0
	case len(args) == 2 && args[0] == "automount":
		if _, err := h.Handle(ctx, storage.Request{Op: "automount", Device: args[1]}); err != nil {
			fmt.Fprintln(env.Stderr, "nasctl storage-helper:", err)
			return 1
		}
		return 0
	}
	fmt.Fprintln(env.Stderr, "usage: nasctl storage-helper [automount DEVICE]")
	return 2
}
