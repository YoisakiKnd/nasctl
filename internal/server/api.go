package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/cachepath"
	"github.com/YoisakiKnd/nasctl/internal/fsutil"
	"github.com/YoisakiKnd/nasctl/internal/placeholder"
	"github.com/YoisakiKnd/nasctl/internal/source"
	"github.com/YoisakiKnd/nasctl/internal/store"
)

// mediaItem is the wire form of one media row.
//
// Every field is either a fact read from the file or a fact recorded by a task.
// Nothing is inferred: an absent capture time is null, an absent coordinate is
// null, and the client is told which timestamp is a fallback so it can say so.
type mediaItem struct {
	ID         int64  `json:"id"`
	Collection string `json:"collection"`
	Name       string `json:"name"`
	RelDir     string `json:"rel_dir"`
	Kind       string `json:"kind"`
	MIME       string `json:"mime"`
	Bytes      int64  `json:"bytes"`

	TakenAt         *string `json:"taken_at"`
	TakenAtUnix     *int64  `json:"taken_at_unix"`
	TakenSrc        string  `json:"taken_src"`
	TakenLabel      string  `json:"taken_label"`
	TakenIsFallback bool    `json:"taken_is_fallback"`
	TakenTZ         string  `json:"taken_tz"`
	TZOffsetMin     *int    `json:"tz_offset_min"`

	Width       int      `json:"width"`
	Height      int      `json:"height"`
	Orientation int      `json:"orientation"`
	Make        string   `json:"make"`
	Model       string   `json:"model"`
	GPS         *gpsPair `json:"gps"`
	DurationMS  *int64   `json:"duration_ms"`

	ProbeState  string `json:"probe_state"`
	ThumbState  string `json:"thumb_state"`
	ThumbURL    string `json:"thumb_url"`
	RawURL      string `json:"raw_url"`
	Playable    bool   `json:"playable"`
	Placeholder bool   `json:"placeholder"`
	Missing     bool   `json:"missing"`
	Note        string `json:"note"`
}

type gpsPair struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type listResponse struct {
	Items         []mediaItem `json:"items"`
	NextCursor    *string     `json:"next_cursor"`
	TotalEstimate int64       `json:"total_estimate"`
	Paused        bool        `json:"paused"`
	Degraded      bool        `json:"degraded"`
	Warnings      []string    `json:"warnings"`
}

// takenLabel describes where a timestamp came from, in words the UI can show.
func takenLabel(src string) string {
	switch src {
	case store.TakenSrcExif:
		return "EXIF DateTimeOriginal"
	case store.TakenSrcContainer:
		return "video container creation time"
	case store.TakenSrcFilename:
		return "parsed from the file name"
	case store.TakenSrcFiletime:
		return "file modification time (fallback)"
	default:
		return "no capture time"
	}
}

func toItem(m store.Media) mediaItem {
	it := mediaItem{
		ID:          m.ID,
		Collection:  m.CollectionCode,
		Name:        m.BaseName,
		RelDir:      m.RelDir,
		Kind:        m.Kind,
		MIME:        m.MIME,
		Bytes:       m.Size,
		TakenSrc:    m.TakenSrc,
		TakenLabel:  takenLabel(m.TakenSrc),
		TakenTZ:     m.TakenTZ,
		Width:       m.Width,
		Height:      m.Height,
		Orientation: m.Orientation,
		Make:        m.Make,
		Model:       m.Model,
		ProbeState:  m.ProbeState,
		ThumbState:  m.ThumbState,
		ThumbURL:    fmt.Sprintf("/api/v1/media/%d/thumb", m.ID),
		RawURL:      fmt.Sprintf("/api/v1/media/%d/raw", m.ID),
		Missing:     m.Missing,
		Note:        m.ProbeReason,
	}
	it.TakenIsFallback = store.TakenSrcIsFallback(m.TakenSrc) && m.TakenSrc != store.TakenSrcNone
	if m.TakenAt != nil {
		// The stored value is either a real UTC instant (when the file carried
		// an offset) or the camera's wall clock recorded as if it were UTC. In
		// both cases the client renders it without shifting it, so a naive time
		// on screen is the time the camera displayed.
		t := time.Unix(*m.TakenAt, 0).UTC()
		s := t.Format("2006-01-02T15:04:05")
		it.TakenAt = &s
		v := *m.TakenAt
		it.TakenAtUnix = &v
	}
	if m.TZOffsetMin != nil {
		v := *m.TZOffsetMin
		it.TZOffsetMin = &v
	}
	if m.Latitude != nil && m.Longitude != nil {
		it.GPS = &gpsPair{Lat: *m.Latitude, Lon: *m.Longitude}
	}
	if m.DurationMS > 0 {
		v := m.DurationMS
		it.DurationMS = &v
	}
	if m.Kind == "video" {
		// A browser can play it if the container is one of the common web
		// formats. We do not transcode, so the client is told honestly which
		// files will play and which will download instead.
		switch strings.ToLower(m.Ext) {
		case "mp4", "m4v", "mov", "webm":
			it.Playable = true
		}
	}
	it.Placeholder = m.ThumbState != store.ThumbOK
	return it
}

// parseListQuery validates and clamps the query parameters.
func (s *Server) parseListQuery(r *http.Request) (store.Filter, store.Cursor, int, error) {
	q := r.URL.Query()
	var f store.Filter
	var cur store.Cursor

	limit := s.Cfg.Server.PageDefault
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return f, cur, 0, errors.New("limit must be a positive integer")
		}
		if n > s.Cfg.Server.PageMax {
			n = s.Cfg.Server.PageMax
		}
		limit = n
	}

	sortMode, err := store.ParseSort(q.Get("sort"))
	if err != nil {
		return f, cur, 0, err
	}
	f.Sort = sortMode

	if code := q.Get("collection"); code != "" {
		col, ok := s.Cfg.Collection(code)
		if !ok {
			return f, cur, 0, fmt.Errorf("unknown collection %q", code)
		}
		_ = col
		cols, cerr := s.Store.Collections(r.Context())
		if cerr != nil {
			return f, cur, 0, cerr
		}
		var id int64
		for _, c := range cols {
			if c.Code == code {
				id = c.ID
				break
			}
		}
		if id == 0 {
			return f, cur, 0, fmt.Errorf("collection %q has not been scanned yet", code)
		}
		f.CollectionID = id
	}

	if k := q.Get("kind"); k != "" {
		switch k {
		case "image", "video", "other", "unsupported":
			f.Kinds = []string{k}
		default:
			return f, cur, 0, errors.New("kind must be image, video or other")
		}
	}
	if txt := q.Get("q"); txt != "" {
		// Refused, not trimmed: a search that silently looked at only part of
		// the text would answer a question nobody asked. The limit is the
		// index's own limit, so the two cannot drift apart again.
		if len(txt) > store.MaxQueryLen {
			return f, cur, 0, fmt.Errorf("q is limited to %d characters", store.MaxQueryLen)
		}
		f.Query = txt
	}
	// A row whose original was absent at the last complete scan is hidden from
	// the gallery by default, because offering a frame that cannot be opened is
	// noise. The flag is exposed rather than implied: an operator who wants to
	// see exactly what went missing after a share failed to mount can ask for
	// it, and the rows are still in the index either way. Nothing here deletes
	// anything, and no scan is ever started by a listing.
	f.IncludeMissing = truthy(q.Get("include_missing"))
	f.OnlyGPS = truthy(q.Get("gps"))
	// "fallback" is the documented name; "unflagged" is kept as an alias so an
	// older client keeps working.
	f.OnlyFallback = truthy(q.Get("fallback")) || truthy(q.Get("unflagged"))

	from := q.Get("from")
	to := q.Get("to")
	if y := q.Get("year"); y != "" && from == "" && to == "" {
		year, err := strconv.Atoi(y)
		if err != nil || year < 1900 || year > 2199 {
			return f, cur, 0, errors.New("year must be a four digit year")
		}
		from = fmt.Sprintf("%04d-01-01", year)
		to = fmt.Sprintf("%04d-12-31", year)
	}
	if m := q.Get("month"); m != "" && from == "" && to == "" {
		if len(m) != 7 || m[4] != '-' {
			return f, cur, 0, errors.New("month must look like 2024-05")
		}
		month, err := strconv.Atoi(m[5:])
		if err != nil || month < 1 || month > 12 {
			return f, cur, 0, errors.New("month must look like 2024-05")
		}
		from = m + "-01"
		end := time.Date(0, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC)
		to = fmt.Sprintf("%s-%02d", m[:4], end.Day())
	}
	if from != "" {
		t, err := time.ParseInLocation("2006-01-02", from, time.UTC)
		if err != nil {
			return f, cur, 0, errors.New("from must look like 2024-05-01")
		}
		v := t.Unix()
		f.From = &v
	}
	if to != "" {
		t, err := time.ParseInLocation("2006-01-02", to, time.UTC)
		if err != nil {
			return f, cur, 0, errors.New("to must look like 2024-05-31")
		}
		v := t.Add(24*time.Hour - time.Second).Unix()
		f.To = &v
	}

	if tok := q.Get("cursor"); tok != "" {
		c, err := store.DecodeCursor(tok)
		if err != nil {
			return f, cur, 0, err
		}
		cur = c
	}
	return f, cur, limit, nil
}

func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, cur, limit, err := s.parseListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	items, next, err := s.Store.ListMedia(ctx, f, cur, limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "query_error", err)
		return
	}

	out := listResponse{Items: make([]mediaItem, 0, len(items)), Warnings: []string{}}
	for _, m := range items {
		out.Items = append(out.Items, toItem(m))
	}
	if next != nil {
		tok := next.Encode()
		out.NextCursor = &tok
	}
	out.TotalEstimate = s.estimate(ctx, f)
	if ps, err := s.Store.PauseState(ctx); err == nil {
		out.Paused = ps.Paused
	}
	if pr, err := s.Store.Pressure(ctx); err == nil && pr.Tripped {
		out.Degraded = true
		out.Warnings = append(out.Warnings,
			"thumbnail generation is disabled after a resource-pressure stop; existing thumbnails still load")
	}
	writeJSON(w, http.StatusOK, out)
}

// estimate returns a cached count for the filter. It is bounded work: a count
// over an indexed predicate, memoised for a few seconds.
func (s *Server) estimate(ctx context.Context, f store.Filter) int64 {
	key := fmt.Sprintf("%d|%s|%s|%v|%v|%v|%v|%v|%v",
		f.CollectionID, strings.Join(f.Kinds, ","), f.Query,
		f.From, f.To, f.OnlyGPS, f.OnlyFallback, f.IncludeMissing, f.Sort)
	if v, ok := s.counts.get(key); ok {
		return v
	}
	n, err := s.Store.CountMedia(ctx, f)
	if err != nil {
		return 0
	}
	if n > 100000 {
		n = 100000
	}
	s.counts.put(key, n)
	return n
}

func (s *Server) handleMediaItem(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	m, err := s.Store.GetMedia(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no media with that id")
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "query_error", err)
		return
	}

	type detail struct {
		Item     mediaItem         `json:"item"`
		Exif     map[string]string `json:"exif"`
		Paths    map[string]string `json:"paths"`
		Warnings []string          `json:"warnings"`
	}
	out := detail{
		Item:     toItem(m),
		Exif:     map[string]string{},
		Paths:    map[string]string{"display": strings.TrimPrefix(m.RelDir+"/"+m.BaseName, "/")},
		Warnings: []string{},
	}
	for k, v := range decodeExtra(m.MetaExtra) {
		out.Exif[k] = v
	}
	if m.Orientation > 1 {
		out.Exif["Orientation"] = fmt.Sprintf("%d", m.Orientation)
	}
	if m.Animated {
		out.Exif["Animated"] = "yes (first frame only)"
	}
	if m.Missing {
		out.Warnings = append(out.Warnings,
			"this file was not present during the last complete scan; the record and its thumbnail are kept until it returns")
	}
	if m.ProbeNote != "" {
		out.Warnings = append(out.Warnings, m.ProbeNote)
	}
	writeJSON(w, http.StatusOK, out)
}

// decodeExtra reads the bounded metadata JSON written by the scanner.
func decodeExtra(s string) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" || len(s) > 64<<10 {
		return out
	}
	var raw struct {
		Tags []struct {
			K string `json:"k"`
			V string `json:"v"`
		} `json:"tags"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return out
	}
	for i, kv := range raw.Tags {
		if i >= 24 {
			break
		}
		if kv.K == "" {
			continue
		}
		out[kv.K] = truncateRunes(kv.V, 512)
	}
	return out
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r) + "…"
}

func truthy(v string) bool { return v == "1" || v == "true" || v == "yes" }

// pathID parses the media id out of the path. The form accepted is narrow on
// purpose: digits only, no sign, no leading zero, at most 18 digits.
//
// strconv.ParseInt on its own would also accept "+1", " 1", "01" and "1_0",
// which are not ids this service ever mints. Every one of them would reach the
// index and either miss (a 404 for a request that is really a bad request) or,
// worse, succeed and imply that several spellings address the same row. There is
// one spelling per row, and anything else is refused before the index is
// touched. Nothing here resembles a path: the id is a number, so no input can
// name a file.
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	const badID = "the media id must be a positive decimal integer"
	if raw == "" || len(raw) > 18 {
		return 0, errors.New(badID)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, errors.New(badID)
		}
	}
	if raw[0] == '0' {
		// Covers "0" and every zero-padded spelling such as "007".
		return 0, errors.New(badID)
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New(badID)
	}
	return id, nil
}

// handleThumb serves a cached thumbnail or an inline placeholder.
//
// The placeholder path is the whole point of the endpoint: when no thumbnail
// exists the client gets an image immediately, and nothing is decoded, queued or
// generated as a side effect of a page view.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	m, err := s.Store.GetMedia(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no media with that id")
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "query_error", err)
		return
	}

	edge := s.Cfg.Decode.ThumbMaxEdge
	if v := r.URL.Query().Get("size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n != edge {
			// Only the single configured size exists. A request for anything
			// else is answered with the configured size: a client cannot ask
			// for a decode at original resolution.
		}
	}

	key := cachepath.Key(m.CollectionCode, m.RelPath)
	path, perr := cachepath.Path(s.Cfg.Paths.CacheDir, key, edge, s.Cfg.Cache.ShardDepth)
	if perr == nil && m.ThumbState == store.ThumbOK {
		if fi, serr := os.Stat(path); serr == nil && fi.Mode().IsRegular() {
			etag := fmt.Sprintf(`"%x-%x"`, fi.Size(), fi.ModTime().UnixNano())
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control",
				fmt.Sprintf("public, max-age=%d", s.Cfg.Server.StaticCacheSeconds))
			if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			tf, oerr := os.Open(path)
			if oerr == nil {
				defer tf.Close()
				http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), tf)
				return
			}
		}
		// The index says there is a thumbnail but the file is gone. That is
		// reported as a placeholder rather than a 500, and the state is left
		// alone: the web service does not repair the index.
	}

	svg := placeholder.SVG(m.BaseName, m.Kind == "video")
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("X-Thumb", "placeholder")
	w.Header().Set("X-Thumb-State", m.ThumbState)
	w.Header().Set("Cache-Control",
		fmt.Sprintf("public, max-age=%d", s.Cfg.Server.ThumbPlaceholderSecs))
	w.Header().Set("Content-Length", strconv.Itoa(len(svg)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(svg)
}

// handleStream serves the original file with Range support.
func (s *Server) handleStream(attachment bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, err := pathID(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		// Streaming is capped separately from ordinary requests: each open
		// original holds a file descriptor and a copy buffer, and a small
		// machine has a limited number of both.
		select {
		case s.stream <- struct{}{}:
			defer func() { <-s.stream }()
		default:
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "busy",
				"too many files are being streamed at once; retry shortly")
			return
		}

		m, err := s.Store.GetMedia(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", "no media with that id")
			return
		}
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "query_error", err)
			return
		}

		path, err := s.resolveOriginal(ctx, m)
		if err != nil {
			// A path that escapes its root is a refusal, not a server error:
			// nothing here is allowed to serve a file from outside a configured
			// collection.
			writeError(w, http.StatusForbidden, "path_refused", err.Error())
			return
		}
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				writeError(w, http.StatusNotFound, "file_missing",
					"the file is recorded in the index but is not present on disk")
				return
			}
			writeError(w, http.StatusForbidden, "open_refused", err.Error())
			return
		}
		defer f.Close()

		fi, err := f.Stat()
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "stat_error", err)
			return
		}
		if !fi.Mode().IsRegular() {
			writeError(w, http.StatusForbidden, "not_a_file", "the path is not a regular file")
			return
		}

		// Content-Type comes from the extension allowlist and is set before
		// ServeContent so that no sniffing happens.
		ct := rawMIMEFor(m.Ext)
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, fi.Size(), fi.ModTime().UnixNano()))
		disposition := "inline"
		if attachment {
			disposition = "attachment"
		}
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`%s; filename*=UTF-8''%s`, disposition, urlPathEscape(m.BaseName)))

		// ServeContent handles Range, If-Range, If-Modified-Since and 416, and
		// copies through a bounded buffer rather than reading the file into
		// memory.
		http.ServeContent(w, r, m.BaseName, fi.ModTime(), f)
	}
}

// resolveOriginal maps an indexed row back to a real path and proves it is
// still inside the configured collection root.
func (s *Server) resolveOriginal(ctx context.Context, m store.Media) (string, error) {
	col, ok := s.Cfg.Collection(m.CollectionCode)
	if !ok {
		return "", fmt.Errorf("collection %q is no longer configured", m.CollectionCode)
	}
	joined, err := fsutil.SafeRelJoin(col.Root, m.RelPath)
	if err != nil {
		return "", err
	}
	real, err := fsutil.ResolveWithin(col.Root, joined)
	if err != nil {
		return "", err
	}
	return real, nil
}

// urlPathEscape escapes a file name for the RFC 5987 filename* parameter.
func urlPathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f, _, _, err := s.parseListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	rows, undated, err := s.Store.Timeline(ctx, f)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, "query_error", err)
		return
	}
	type month struct {
		Month int   `json:"month"`
		Count int64 `json:"count"`
	}
	type year struct {
		Year   int     `json:"year"`
		Count  int64   `json:"count"`
		Months []month `json:"months"`
	}
	out := struct {
		Years   []year `json:"years"`
		Undated int64  `json:"undated"`
		Paused  bool   `json:"paused"`
	}{Years: []year{}}

	idx := map[int]int{}
	for _, r := range rows {
		i, ok := idx[r.Year]
		if !ok {
			out.Years = append(out.Years, year{Year: r.Year})
			i = len(out.Years) - 1
			idx[r.Year] = i
		}
		out.Years[i].Count += r.Count
		out.Years[i].Months = append(out.Years[i].Months, month{Month: r.Month, Count: r.Count})
	}
	out.Undated = undated
	if ps, err := s.Store.PauseState(ctx); err == nil {
		out.Paused = ps.Paused
	}
	writeJSON(w, http.StatusOK, out)
}

// sourceHealth is a read-only, cheap check the status panel can show. It never
// resolves paths beyond the configured roots and never touches a media file.
func (s *Server) sourceHealth(ctx context.Context) []string {
	var out []string
	cols, err := s.Store.Collections(ctx)
	if err != nil {
		return out
	}
	for _, c := range cols {
		cfgCol, ok := s.Cfg.Collection(c.Code)
		if !ok {
			continue
		}
		sc := source.Verify(cfgCol, s.Cfg.Paths.StateDir, c.ExpectFSID)
		if !sc.OK {
			out = append(out, fmt.Sprintf("collection %s: %s", c.Code, sc.Fatal))
		}
	}
	return out
}
