package anime

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"time"
)

type MetadataOptions struct {
	Subscription int64 `json:"subscription"`
	Limit        int   `json:"limit"`
	AfterID      int64 `json:"after_id"`
	Replace      bool  `json:"replace"`
}
type MetadataResult struct {
	Written  int   `json:"written"`
	Skipped  int   `json:"skipped"`
	Episodes int   `json:"episodes"`
	NextID   int64 `json:"next_id,omitempty"`
}
type metadataFile struct {
	path string
	data []byte
}

func xmlMetadata(value any) ([]byte, error) {
	b, err := xml.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}

func (e *Engine) seriesMetadata(ctx context.Context, sub Subscription) ([]metadataFile, error) {
	var remote struct {
		ID           int64  `json:"id"`
		OriginalName string `json:"original_name"`
		Overview     string `json:"overview"`
		FirstAirDate string `json:"first_air_date"`
		PosterPath   string `json:"poster_path"`
	}
	if sub.TMDBID > 0 && e.cfg.TMDBToken != "" {
		if err := e.tmdbJSON(ctx, fmt.Sprintf("tv/%d?language=zh-CN", sub.TMDBID), &remote); err != nil {
			return nil, err
		}
		if remote.ID != sub.TMDBID {
			return nil, inputError("TMDB 返回了不同剧集，未更新元数据")
		}
	}
	nfo := struct {
		XMLName       xml.Name `xml:"tvshow"`
		Title         string   `xml:"title"`
		OriginalTitle string   `xml:"originaltitle,omitempty"`
		Plot          string   `xml:"plot,omitempty"`
		Premiered     string   `xml:"premiered,omitempty"`
		ID            struct {
			Type    string `xml:"type,attr"`
			Default string `xml:"default,attr"`
			Value   string `xml:",chardata"`
		} `xml:"uniqueid,omitempty"`
	}{Title: sub.Title, OriginalTitle: metadataText(remote.OriginalName, 200), Plot: metadataText(remote.Overview, 20000), Premiered: metadataText(remote.FirstAirDate, 10)}
	if sub.TMDBID > 0 {
		nfo.ID.Type = "tmdb"
		nfo.ID.Default = "true"
		nfo.ID.Value = strconv.FormatInt(sub.TMDBID, 10)
	}
	data, err := xmlMetadata(nfo)
	if err != nil {
		return nil, err
	}
	files := []metadataFile{{path.Join(sub.Title, "tvshow.nfo"), data}}
	if remote.PosterPath != "" {
		if !regexp.MustCompile(`^/[A-Za-z0-9]+\.jpg$`).MatchString(remote.PosterPath) {
			return nil, inputError("TMDB 封面路径无效")
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://image.tmdb.org/t/p/w500"+remote.PosterPath, nil)
		client := *e.feedClient
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return inputError("封面重定向已拒绝") }
		client.Timeout = 12 * time.Second
		res, err := client.Do(req)
		if err != nil {
			return nil, inputError("封面读取失败")
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, MaxFeedBytes+1))
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || len(b) > MaxFeedBytes || len(b) < 3 || b[0] != 0xff || b[1] != 0xd8 || b[2] != 0xff {
			return nil, inputError("封面无效或超过 2 MiB")
		}
		files = append(files, metadataFile{path.Join(sub.Title, "poster.jpg"), b})
	}
	return files, nil
}

func (e *Engine) episodeMetadata(ctx context.Context, sub Subscription, ep Episode) (metadataFile, error) {
	file := metadataFile{path: ep.LibraryPath[:len(ep.LibraryPath)-len(path.Ext(ep.LibraryPath))] + ".nfo"}
	nfo := struct {
		XMLName   xml.Name `xml:"episodedetails"`
		Title     string   `xml:"title"`
		ShowTitle string   `xml:"showtitle"`
		Season    int      `xml:"season"`
		Episode   int      `xml:"episode"`
		Plot      string   `xml:"plot,omitempty"`
		Aired     string   `xml:"aired,omitempty"`
	}{Title: ep.Title, ShowTitle: sub.Title, Season: ep.Season, Episode: ep.Number}
	if sub.TMDBID > 0 && e.cfg.TMDBToken != "" {
		var remote struct {
			Name     string `json:"name"`
			Overview string `json:"overview"`
			AirDate  string `json:"air_date"`
			Season   int    `json:"season_number"`
			Episode  int    `json:"episode_number"`
		}
		if err := e.tmdbJSON(ctx, fmt.Sprintf("tv/%d/season/%d/episode/%d?language=zh-CN", sub.TMDBID, ep.Season, ep.Number), &remote); err != nil {
			return file, err
		}
		if remote.Season != ep.Season || remote.Episode != ep.Number {
			return file, inputError("TMDB 返回了不同集数，未更新单集元数据")
		}
		if remote.Name != "" {
			nfo.Title = metadataText(remote.Name, 200)
		}
		nfo.Plot = metadataText(remote.Overview, 20000)
		nfo.Aired = metadataText(remote.AirDate, 10)
	}
	data, err := xmlMetadata(nfo)
	file.data = data
	return file, err
}

func (e *Engine) RefreshMetadata(ctx context.Context, opt MetadataOptions, report func(int64, int64)) (MetadataResult, error) {
	out := MetadataResult{}
	if opt.Subscription < 1 || opt.AfterID < 0 || opt.Limit < 1 || opt.Limit > 50 {
		return out, inputError("请选择订阅及 1–50 集的处理上限")
	}
	if !e.mu.TryLock() {
		return out, ErrBusy
	}
	defer e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := e.admit(ctx); err != nil {
		return out, err
	}
	sub, err := e.Store.Subscription(ctx, opt.Subscription)
	if err != nil {
		return out, err
	}
	// Keyset batches permit continuation without reading the full library.
	rows, err := e.Store.db.QueryContext(ctx, `SELECT e.id FROM episodes e WHERE subscription_id=? AND state='archived' AND id>? ORDER BY id LIMIT ?`, sub.ID, opt.AfterID, opt.Limit+1)
	if err != nil {
		return out, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	more := len(ids) > opt.Limit
	if more {
		ids = ids[:opt.Limit]
	}
	files, err := e.seriesMetadata(ctx, *sub)
	if err != nil {
		return out, err
	}
	if err = e.libraryRoot.MkdirAll(sub.Title, 0750); err != nil {
		return out, err
	}
	apply := func(file metadataFile) error {
		if err := e.admit(ctx); err != nil {
			return err
		}
		written, err := e.updateMetadataFile(ctx, file.path, file.data, opt.Replace)
		if err == nil {
			if written {
				out.Written++
			} else {
				out.Skipped++
			}
		}
		return err
	}
	for _, file := range files {
		if err = apply(file); err != nil {
			return out, err
		}
	}
	for _, id := range ids {
		ep, err := e.Store.Episode(ctx, id)
		if err != nil {
			return out, err
		}
		f, info, err := e.OpenVideo(ctx, id)
		if err != nil {
			return out, inputError("已入库视频暂不可用，未更新这一集")
		}
		f.Close()
		if !info.Mode().IsRegular() {
			return out, inputError("媒体文件无效")
		}
		file, err := e.episodeMetadata(ctx, *sub, ep)
		if err != nil {
			return out, err
		}
		if err = apply(file); err != nil {
			return out, err
		}
		out.Episodes++
		out.NextID = id
		if report != nil {
			report(int64(out.Episodes), int64(len(ids)))
		}
	}
	if !more {
		out.NextID = 0
	}
	return out, nil
}
