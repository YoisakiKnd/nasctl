package anime

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const maxSubtitleBytes = 1 << 20

type PlaybackUpdate struct {
	Position *float64 `json:"position"`
	Duration *float64 `json:"duration"`
	Watched  *bool    `json:"watched"`
}
type WatchState struct {
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Watched   bool    `json:"watched"`
	UpdatedAt int64   `json:"updated_at"`
}

// Playback bookkeeping never changes download state and needs no downloader.
// Pausing a subscription does not prevent watching an already archived episode.
func (s *Store) SavePlayback(ctx context.Context, id int64, update PlaybackUpdate) (WatchState, error) {
	var state WatchState
	if update.Position == nil && update.Duration == nil && update.Watched == nil {
		return state, inputError("没有提供观看状态")
	}
	if (update.Position == nil) != (update.Duration == nil) {
		return state, inputError("播放位置和时长必须一起提供")
	}
	if update.Position != nil {
		p, d := *update.Position, *update.Duration
		if math.IsNaN(p) || math.IsInf(p, 0) || math.IsNaN(d) || math.IsInf(d, 0) || p < 0 || d < 0 || d > 86400 || p > d {
			return state, inputError("播放位置或时长无效")
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	defer tx.Rollback()
	var stage string
	err = tx.QueryRowContext(ctx, `SELECT state,watch_position,watch_duration,watched,watch_updated_at FROM episodes WHERE id=?`, id).Scan(&stage, &state.Position, &state.Duration, &state.Watched, &state.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, ErrNotFound
		}
		return state, err
	}
	if stage != "archived" {
		return state, inputError("只有已入库剧集可以记录观看状态")
	}
	if update.Position != nil {
		state.Position, state.Duration = *update.Position, *update.Duration
		if state.Duration > 0 && state.Position/state.Duration >= 0.95 {
			state.Watched = true
		}
	}
	if update.Watched != nil {
		state.Watched = *update.Watched
	}
	state.UpdatedAt = time.Now().Unix()
	_, err = tx.ExecContext(ctx, `UPDATE episodes SET watch_position=?,watch_duration=?,watched=?,watch_updated_at=? WHERE id=?`, state.Position, state.Duration, state.Watched, state.UpdatedAt, id)
	if err != nil {
		return state, err
	}
	return state, tx.Commit()
}

type Subtitle struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Format   string `json:"format"`
	Playable bool   `json:"playable"`
}
type Playback struct {
	Episode   Episode    `json:"episode"`
	Subtitles []Subtitle `json:"subtitles"`
	Truncated bool       `json:"truncated"`
}

func (e *Engine) Playback(ctx context.Context, id int64) (Playback, error) {
	result := Playback{Subtitles: []Subtitle{}}
	ep, err := e.Store.Episode(ctx, id)
	if err != nil {
		return result, err
	}
	if ep.State != "archived" {
		return result, ErrNotFound
	}
	p, err := safeRelative(ep.LibraryPath)
	if err != nil {
		return result, err
	}
	result.Episode = ep
	dir, err := e.libraryRoot.OpenFile(path.Dir(p), os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return result, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) {
		return result, err
	}
	if len(entries) > 4096 {
		entries = entries[:4096]
		result.Truncated = true
	}
	stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
	for _, entry := range entries {
		name := entry.Name()
		ext := strings.ToLower(path.Ext(name))
		if entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(name, stem+".") || !subtitleFile(name) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		label := strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(name, stem), path.Ext(name)), ".")
		if label == "" {
			label = "默认字幕"
		}
		result.Subtitles = append(result.Subtitles, Subtitle{Name: name, Label: label, Format: strings.TrimPrefix(ext, "."), Playable: (ext == ".srt" || ext == ".vtt") && info.Size() <= maxSubtitleBytes})
		if len(result.Subtitles) == 30 {
			result.Truncated = true
			break
		}
	}
	return result, nil
}

// OpenSubtitle binds the requested sibling to one archived episode. Neither a
// root path nor another episode's subtitle can be supplied by the caller.
func (e *Engine) OpenSubtitle(ctx context.Context, id int64, name string) (*os.File, os.FileInfo, error) {
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
	stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
	if name != path.Base(name) || strings.ContainsAny(name, "\\\x00") || !strings.HasPrefix(name, stem+".") || !subtitleFile(name) {
		return nil, nil, ErrNotFound
	}
	return e.openLibraryFile(path.Join(path.Dir(p), name), 0)
}
func (e *Engine) PosterAvailable(sub Subscription) bool {
	info, err := e.libraryRoot.Lstat(path.Join(sub.Title, "poster.jpg"))
	return err == nil && info.Mode().IsRegular() && info.Size() >= 3 && info.Size() <= MaxFeedBytes
}
func (e *Engine) PosterVersion(sub Subscription) string {
	info, err := e.libraryRoot.Lstat(path.Join(sub.Title, "poster.jpg"))
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxFeedBytes {
		return ""
	}
	return strconv.FormatInt(info.ModTime().UnixNano(), 10)
}
func (e *Engine) OpenPoster(ctx context.Context, id int64) (*os.File, os.FileInfo, error) {
	sub, err := e.Store.Subscription(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	f, info, err := e.openLibraryFile(path.Join(sub.Title, "poster.jpg"), MaxFeedBytes)
	if err != nil {
		return nil, nil, err
	}
	var header [3]byte
	if _, err = io.ReadFull(f, header[:]); err != nil || header != [3]byte{0xff, 0xd8, 0xff} {
		f.Close()
		return nil, nil, ErrNotFound
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}
func (e *Engine) openLibraryFile(p string, maxBytes int64) (*os.File, os.FileInfo, error) {
	clean, err := safeRelative(p)
	if err != nil {
		return nil, nil, err
	}
	f, err := e.libraryRoot.OpenFile(clean, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || (maxBytes > 0 && info.Size() > maxBytes) {
		f.Close()
		return nil, nil, ErrNotFound
	}
	return f, info, nil
}

var srtTimestamp = regexp.MustCompile(`(?m)^(\d{2}:\d{2}:\d{2}),(\d{3})([ \t]+-->[ \t]+)(\d{2}:\d{2}:\d{2}),(\d{3})`)

func SubtitleVTT(name string, r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSubtitleBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSubtitleBytes {
		return nil, inputError("字幕超过 1 MiB，请下载原文件")
	}
	if !utf8.Valid(b) || strings.Count(string(b), "\n") > 20000 {
		return nil, inputError("字幕不是有效的 UTF-8 文本或行数过多，请下载原文件")
	}
	text := strings.ReplaceAll(strings.TrimPrefix(string(b), "\ufeff"), "\r\n", "\n")
	switch strings.ToLower(path.Ext(name)) {
	case ".vtt":
		if !strings.HasPrefix(text, "WEBVTT\n") && !strings.HasPrefix(text, "WEBVTT ") {
			return nil, inputError("WebVTT 字幕头无效")
		}
		return []byte(text), nil
	case ".srt":
		if !srtTimestamp.MatchString(text) {
			return nil, inputError("SRT 字幕缺少有效时间轴")
		}
		return []byte("WEBVTT\n\n" + srtTimestamp.ReplaceAllString(text, `${1}.${2}${3}${4}.${5}`)), nil
	default:
		return nil, inputError("此字幕格式仅支持下载，不支持浏览器内播放")
	}
}
