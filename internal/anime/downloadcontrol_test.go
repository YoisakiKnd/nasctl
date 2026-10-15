package anime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQBittorrentControlVersionEndpointsAndSingleHash(t *testing.T) {
	hash := strings.Repeat("a", 40)
	for _, version := range []string{"v4.6.7", "v5.0.2"} {
		calls := []string{}
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v2/app/version" {
				io.WriteString(w, version)
				return
			}
			if r.Method != "POST" {
				t.Error("mutating GET")
			}
			r.ParseForm()
			if r.Form.Get("hashes") != hash || len(r.Form) != 1 {
				t.Error("unscoped control", r.Form)
			}
			calls = append(calls, strings.TrimPrefix(r.URL.Path, "/api/v2/torrents/"))
		}))
		q := NewQBittorrent(ts.URL, "", "")
		for _, action := range []string{"pause", "resume", "recheck"} {
			if err := q.Control(context.Background(), hash, action); err != nil {
				t.Fatal(err)
			}
		}
		want := "pause,resume,recheck"
		if strings.HasPrefix(version, "v5") {
			want = "stop,start,recheck"
		}
		if strings.Join(calls, ",") != want {
			t.Fatal(version, calls)
		}
		for _, bad := range []string{"all", hash + "|" + hash, "invalid", strings.Repeat("g", 40)} {
			if err := q.Control(context.Background(), bad, "pause"); err == nil {
				t.Fatal("unsafe hash", bad)
			}
		}
		if err := q.Control(context.Background(), hash, "delete"); err == nil {
			t.Fatal("undeclared control")
		}
		ts.Close()
	}
}

type controlledDownloader struct {
	*fakeDownloader
	calls         int
	rejectControl bool
}

func (d *controlledDownloader) Control(ctx context.Context, hash, action string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.rejectControl {
		return errors.New("ambiguous transport failure")
	}
	for key, t := range d.torrents {
		if t.Hash == hash {
			if action == "pause" {
				t.State = "stoppedDL"
			} else if action == "resume" {
				t.State = "downloading"
			} else {
				t.State = "checkingDL"
			}
			d.torrents[key] = t
			return nil
		}
	}
	return errors.New("not found")
}

func TestDownloadControlReconcilesTagAndNeverResubmitsOrDeletes(t *testing.T) {
	e, down, _ := newEngine(t, sampleRSS)
	ctx := context.Background()
	if _, err := e.Refresh(ctx, 0); err != nil {
		t.Fatal(err)
	}
	rows, _ := e.Store.Episodes(ctx, "discovered", 50, 0)
	ep := rows[0]
	if _, err := e.ControlDownload(ctx, ep.ID, "pause"); err == nil {
		t.Fatal("undownloaded episode controlled")
	}
	if err := e.Queue(ctx, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, ep.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	tag := fmt.Sprintf("nasctl-episode-%d", ep.ID)
	tor := down.torrents[tag]
	tor.Hash = strings.Repeat("a", 40)
	down.torrents[tag] = tor
	control := &controlledDownloader{fakeDownloader: down}
	e.downloader = control
	paused, err := e.ControlDownload(ctx, ep.ID, "pause")
	if err != nil || paused.State != "paused" {
		t.Fatal(paused, err)
	}
	if _, err := e.Sync(ctx, ep.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	if row, _ := e.Store.Episode(ctx, ep.ID); row.State != "paused" {
		t.Fatal("paused torrent not preserved", row)
	}
	resumed, err := e.ControlDownload(ctx, ep.ID, "resume")
	if err != nil || resumed.State != "downloading" {
		t.Fatal(resumed, err)
	}
	if _, err := e.ControlDownload(ctx, ep.ID, "recheck"); err != nil {
		t.Fatal(err)
	}
	if down.adds != 1 || control.calls != 3 {
		t.Fatal("control resubmitted download", down.adds, control.calls)
	}
	control.rejectControl = true
	if _, err := e.ControlDownload(ctx, ep.ID, "pause"); err == nil {
		t.Fatal("ambiguous control claimed success")
	}
	if row, _ := e.Store.Episode(ctx, ep.ID); row.State == "paused" {
		t.Fatal("unconfirmed pause persisted")
	}
	control.rejectControl = false
	tor = down.torrents[tag]
	tor.Hash = strings.Repeat("b", 40)
	down.torrents[tag] = tor
	if _, err := e.ControlDownload(ctx, ep.ID, "resume"); err == nil {
		t.Fatal("replaced torrent controlled")
	}
	delete(down.torrents, tag)
	if _, err := e.ControlDownload(ctx, ep.ID, "resume"); err == nil {
		t.Fatal("missing torrent resubmitted")
	}
	if down.adds != 1 {
		t.Fatal(down.adds)
	}
}
