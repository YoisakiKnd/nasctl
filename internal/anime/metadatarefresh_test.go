package anime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func metadataRig(t *testing.T) (*Engine, []Episode, string) {
	t.Helper()
	e, _, cfg := newEngine(t, sampleRSS)
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := e.Store.Episodes(ctx, "", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		rows[i].State = "archived"
		rows[i].LibraryPath = path.Join("Test Show", "Season 1", fmt.Sprintf("Test Show S01E%02d.mp4", rows[i].Number))
		if err := e.libraryRoot.MkdirAll(path.Dir(rows[i].LibraryPath), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.LibraryDir, rows[i].LibraryPath), []byte("original video bytes"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := e.Store.updateEpisode(ctx, rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	return e, rows, cfg.LibraryDir
}

func TestMetadataRefreshBatchesPreservesMediaAndCanRestorePreviousVersion(t *testing.T) {
	e, episodes, library := metadataRig(t)
	ctx := context.Background()
	old := []byte("<tvshow><title>custom old metadata</title></tvshow>")
	series := filepath.Join(library, "Test Show", "tvshow.nfo")
	if err := os.WriteFile(series, old, 0640); err != nil {
		t.Fatal(err)
	}
	first, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 1}, nil)
	if err != nil || first.Episodes != 1 || first.NextID == 0 || first.Written != 1 || first.Skipped != 1 {
		t.Fatal(first, err)
	}
	if data, _ := os.ReadFile(series); string(data) != string(old) {
		t.Fatal("fill missing overwrote custom metadata")
	}
	last, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 1, AfterID: first.NextID}, nil)
	if err != nil || last.NextID != 0 || last.Episodes != 1 {
		t.Fatal(last, err)
	}
	updated, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 50, Replace: true}, nil)
	if err != nil || updated.Written != 1 || updated.Episodes != 2 {
		t.Fatal(updated, err)
	}
	backups, err := e.MetadataBackups(ctx)
	if err != nil || len(backups) != 1 {
		t.Fatal(backups, err)
	}
	_, data, err := e.ExportMetadataBackup(ctx, backups[0].ID)
	if err != nil || string(data) != string(old) {
		t.Fatal(string(data), err)
	}
	if err := e.RestoreMetadata(ctx, backups[0].ID); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(series); string(data) != string(old) {
		t.Fatal("restore did not restore old bytes")
	}
	if err := e.DeleteMetadataBackup(ctx, backups[0].ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong confirmation deleted backup")
	}
	current, err := e.MetadataBackups(ctx)
	if err != nil || len(current) != 2 {
		t.Fatal(current, err)
	}
	if err := e.DeleteMetadataBackup(ctx, current[0].ID, current[0].SHA256); err != nil {
		t.Fatal(err)
	}
	for _, ep := range episodes {
		data, err := os.ReadFile(filepath.Join(library, ep.LibraryPath))
		if err != nil || string(data) != "original video bytes" {
			t.Fatal("video changed", string(data), err)
		}
		row, err := e.Store.Episode(ctx, ep.ID)
		if err != nil || row.State != "archived" || row.LibraryPath != ep.LibraryPath {
			t.Fatal(row, err)
		}
	}
}

func TestMetadataRefreshUsesFixedTMDBSeriesAndEpisodeAndEscapesXML(t *testing.T) {
	e, _, library := metadataRig(t)
	ctx := context.Background()
	sub, _ := e.Store.Subscription(ctx, 1)
	sub.TMDBID = 42
	if _, err := e.SaveSubscription(ctx, *sub); err != nil {
		t.Fatal(err)
	}
	e.cfg.TMDBToken = "private-metadata-token"
	requests := 0
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "api.themoviedb.org" || r.Header.Get("Authorization") != "Bearer private-metadata-token" {
			t.Fatal("credentials crossed origin", r.URL)
		}
		body := `{"id":42,"original_name":"Original <name>","overview":"Plot & description"}`
		if strings.Contains(r.URL.Path, "/episode/") {
			number := 1
			if strings.HasSuffix(r.URL.Path, "/2") {
				number = 2
			}
			body = fmt.Sprintf(`{"name":"Episode <title>","overview":"Story & notes","season_number":1,"episode_number":%d}`, number)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	result, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 50, Replace: true}, nil)
	if err != nil || result.Written != 3 || requests != 3 {
		t.Fatal(result, requests, err)
	}
	data, err := os.ReadFile(filepath.Join(library, "Test Show", "Season 1", "Test Show S01E01.nfo"))
	if err != nil || !strings.Contains(string(data), "Episode &lt;title&gt;") || !strings.Contains(string(data), "Story &amp; notes") {
		t.Fatal(string(data), err)
	}
}

func TestMetadataReplacementRejectsSymlinkOversizeAndExhaustedBackupBudget(t *testing.T) {
	e, _, library := metadataRig(t)
	ctx := context.Background()
	target := filepath.Join(library, "Test Show", "tvshow.nfo")
	outside := filepath.Join(t.TempDir(), "secret.nfo")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 1, Replace: true}, nil); err == nil {
		t.Fatal("symlink overwritten")
	}
	if data, _ := os.ReadFile(outside); string(data) != "secret" {
		t.Fatal("outside metadata changed")
	}
	os.Remove(target)
	if err := os.WriteFile(target, make([]byte, MaxFeedBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 1, Replace: true}, nil); err == nil {
		t.Fatal("oversized old metadata overwritten")
	}
	if err := os.WriteFile(target, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.metadataDir, 0700); err != nil {
		t.Fatal(err)
	}
	large, err := os.Create(filepath.Join(e.metadataDir, "budget-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(metadataBackupBytes); err != nil {
		t.Fatal(err)
	}
	large.Close()
	if _, err := e.RefreshMetadata(ctx, MetadataOptions{Subscription: 1, Limit: 1, Replace: true}, nil); err == nil {
		t.Fatal("backup quota ignored")
	}
	if data, _ := os.ReadFile(target); string(data) != "keep me" {
		t.Fatal("target replaced without backup")
	}
}
