package anime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func mediaFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mkv", ".mp4", ".avi", ".webm", ".m4v", ".ts", ".mov":
		return true
	}
	return false
}
func subtitleFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".ssa", ".vtt":
		return true
	}
	return false
}
func safeRelative(p string) (string, error) {
	if filepath.IsAbs(p) || strings.ContainsAny(p, "\x00\\") {
		return "", errors.New("anime: unsafe downloader file path")
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", errors.New("anime: downloader file leaves download directory")
	}
	return c, nil
}

func (e *Engine) archive(ctx context.Context, ep *Episode, torrent Torrent, files []TorrentFile) error {
	if len(files) > 500 {
		return errors.New("anime: torrent has more than 500 files")
	}
	base, err := e.localDownloadPath(torrent.SavePath)
	if err != nil {
		return err
	}
	if base != "." {
		if _, err = safeRelative(filepath.ToSlash(base)); err != nil {
			return err
		}
	}
	video := ""
	subtitles := []string{}
	for _, f := range files {
		if f.Progress < 1 {
			continue
		}
		name, err := safeRelative(f.Name)
		if err != nil {
			return err
		}
		if mediaFile(name) {
			if video != "" {
				return errors.New("anime: season packs or multiple-video releases need manual organization")
			}
			video = name
		} else if subtitleFile(name) {
			subtitles = append(subtitles, name)
		}
	}
	if video == "" {
		return errors.New("anime: completed torrent has no complete video")
	}
	series := ep.Series
	dir := path.Join(series, fmt.Sprintf("Season %d", ep.Season))
	if err = e.libraryRoot.MkdirAll(dir, 0750); err != nil {
		return err
	}
	stem := fmt.Sprintf("%s S%02dE%02d", series, ep.Season, ep.Number)
	dest := path.Join(dir, stem+strings.ToLower(path.Ext(video)))
	source := path.Join(filepath.ToSlash(base), video)
	if err = e.archiveFile(ctx, source, dest); err != nil {
		return err
	}
	// Preserve subtitle language/release suffixes without overwriting siblings.
	for _, sub := range subtitles {
		videoStem := strings.TrimSuffix(path.Base(video), path.Ext(video))
		subBase := path.Base(sub)
		if !strings.HasPrefix(subBase, videoStem+".") {
			continue
		}
		suffix := strings.TrimPrefix(subBase, videoStem)
		if err = e.archiveFile(ctx, path.Join(filepath.ToSlash(base), sub), path.Join(dir, stem+suffix)); err != nil {
			return err
		}
	}
	nfo := struct {
		XMLName   xml.Name `xml:"episodedetails"`
		Title     string   `xml:"title"`
		ShowTitle string   `xml:"showtitle"`
		Season    int      `xml:"season"`
		Episode   int      `xml:"episode"`
	}{Title: ep.Title, ShowTitle: series, Season: ep.Season, Episode: ep.Number}
	b, err := xml.MarshalIndent(nfo, "", "  ")
	if err != nil {
		return err
	}
	if err = e.writeMetadata(path.Join(dir, stem+".nfo"), append([]byte(xml.Header), b...)); err != nil {
		return err
	}
	ep.LibraryPath = dest
	return nil
}

func (e *Engine) archiveFile(ctx context.Context, source, dest string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := e.downloadRoot.OpenFile(source, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("anime: source cannot be opened: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("anime: only regular files can be archived")
	}
	if old, err := e.libraryRoot.Lstat(dest); err == nil {
		if os.SameFile(fi, old) {
			return nil
		}
		// A completed cross-device copy is recognized by our source manifest,
		// not just file size (unrelated files can have the same size).
		return e.verifyCopy(source, dest, fi, old)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	a, err := e.downloadRoot.Open(path.Dir(source))
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := e.libraryRoot.Open(path.Dir(dest))
	if err != nil {
		return err
	}
	defer b.Close()
	err = unix.Linkat(int(a.Fd()), path.Base(source), int(b.Fd()), path.Base(dest), 0)
	if err == nil {
		linked, serr := e.libraryRoot.Lstat(dest)
		if serr != nil {
			return serr
		}
		if !linked.Mode().IsRegular() || !os.SameFile(fi, linked) {
			return errors.New("anime: source changed during archive; inspect destination")
		}
		return nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("anime: hard link failed: %w", err)
	}
	if fi.Size() > 8<<30 {
		return errors.New("anime: cross-device copy exceeds 8 GiB; use a library on the same filesystem")
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(dest), ".nasctl-archive-"+hex.EncodeToString(token[:]))
	out, err := e.libraryRoot.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	defer e.libraryRoot.Remove(tmp)
	_, err = io.CopyBuffer(out, contextReader{ctx: ctx, r: io.LimitReader(f, fi.Size()+1)}, make([]byte, 64<<10))
	if err == nil {
		var after os.FileInfo
		after, err = f.Stat()
		if err == nil && (after.Size() != fi.Size() || !after.ModTime().Equal(fi.ModTime())) {
			err = errors.New("anime: source changed during copy")
		}
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	// Publish by hard link on the destination filesystem: atomic, no clobber.
	if err = e.libraryRoot.Link(tmp, dest); err != nil {
		return err
	}
	manifest := fmt.Sprintf("%s\n%d\n%d\n", source, fi.Size(), fi.ModTime().UnixNano())
	return e.writeMetadata(dest+".nasctl-source", []byte(manifest))
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}
func (e *Engine) verifyCopy(source, dest string, sourceInfo, destInfo os.FileInfo) error {
	if !destInfo.Mode().IsRegular() || destInfo.Size() != sourceInfo.Size() {
		return errors.New("anime: archive destination already exists")
	}
	f, err := e.libraryRoot.Open(dest + ".nasctl-source")
	if err != nil {
		return errors.New("anime: archive destination already exists")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8192))
	if err != nil {
		return err
	}
	expected := fmt.Sprintf("%s\n%d\n%d\n", source, sourceInfo.Size(), sourceInfo.ModTime().UnixNano())
	if string(b) != expected {
		return errors.New("anime: archive destination does not match source")
	}
	return nil
}
func (e *Engine) writeMetadata(dest string, data []byte) error {
	if _, err := e.libraryRoot.Lstat(dest); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	tmp := path.Join(path.Dir(dest), ".nasctl-meta-"+hex.EncodeToString(token[:]))
	f, err := e.libraryRoot.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	defer e.libraryRoot.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	err = e.libraryRoot.Link(tmp, dest)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	return err
}

// OpenVideo resolves an archived episode by ID, never by a caller-supplied path.
func (e *Engine) OpenVideo(ctx context.Context, id int64) (*os.File, os.FileInfo, error) {
	ep, err := e.Store.Episode(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if ep.State != "archived" {
		return nil, nil, ErrNotFound
	}
	p, err := safeRelative(ep.LibraryPath)
	if err != nil {
		return nil, nil, err
	}
	f, err := e.libraryRoot.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, errors.New("anime: library entry is not a regular file")
	}
	return f, fi, nil
}
