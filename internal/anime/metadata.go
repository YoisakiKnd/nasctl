package anime

import (
	"context"
	"errors"
	"os"
	"path"
)

// Automatic scraping only fills a missing series record. Explicit library
// refresh uses the same bounded builder with per-file previous-version backups.
func (e *Engine) scrape(ctx context.Context, sub Subscription) error {
	dest := path.Join(sub.Title, "tvshow.nfo")
	if _, err := e.libraryRoot.Lstat(dest); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	files, err := e.seriesMetadata(ctx, sub)
	if err != nil {
		return err
	}
	if err = e.libraryRoot.MkdirAll(sub.Title, 0750); err != nil {
		return err
	}
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if err := e.writeMetadata(file.path, file.data); err != nil {
			return err
		}
	}
	return nil
}
