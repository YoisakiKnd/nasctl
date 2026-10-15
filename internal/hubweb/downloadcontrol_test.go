package hubweb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
)

func TestSingleEpisodeDownloaderControlThroughRealHTTPAndJobGates(t *testing.T) {
	r := newRig(t, nil)
	base := t.TempDir()
	hash := strings.Repeat("a", 40)
	var mu sync.Mutex
	state := "downloading"
	calls := []string{}
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.URL.Path {
		case "/api/v2/app/version":
			io.WriteString(w, "v5.0.2")
		case "/api/v2/torrents/info":
			if req.URL.Query().Get("tag") != "nasctl-episode-1" {
				t.Error("control was not scoped by tag")
			}
			json.NewEncoder(w).Encode([]anime.Torrent{{Hash: hash, State: state, Progress: .2}})
		case "/api/v2/torrents/stop", "/api/v2/torrents/start", "/api/v2/torrents/recheck":
			req.ParseForm()
			if req.Form.Get("hashes") != hash {
				t.Error("wrong torrent", req.Form)
			}
			calls = append(calls, req.URL.Path)
			if strings.HasSuffix(req.URL.Path, "stop") {
				state = "stoppedDL"
			} else if strings.HasSuffix(req.URL.Path, "start") {
				state = "downloading"
			} else {
				state = "checkingDL"
			}
		default:
			t.Error("unexpected downloader side effect", req.URL.Path)
			w.WriteHeader(400)
		}
	}))
	defer qb.Close()
	download, library := filepath.Join(base, "download"), filepath.Join(base, "library")
	for _, p := range []string{download, library} {
		if err := os.Mkdir(p, 0750); err != nil {
			t.Fatal(err)
		}
	}
	e, err := anime.Open(config.Anime{Enabled: true, QBittorrentURL: qb.URL, DownloadDir: download, LibraryDir: library}, filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	r.srv.Anime = e
	t.Cleanup(func() { r.srv.WaitWork(); e.Close() })
	sub, err := e.SaveSubscription(context.Background(), anime.Subscription{Title: "Control fixture", FeedURL: "https://example.com/feed", Season: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(base, "state", "anime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result, err := db.Exec(`INSERT INTO episodes(subscription_id,guid,title,url,number,season,state,hash,updated_at,ever_submitted) VALUES(?,'fixture','Control fixture - 01','https://example.com/a.torrent',1,1,'downloading',?,1,1)`, sub.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	endpoint := fmt.Sprintf("/api/v1/anime/episodes/%d/download", id)
	for _, tc := range []struct{ action, want string }{{"pause", "paused"}, {"resume", "downloading"}, {"recheck", "checking"}} {
		code, b := fileRequest(t, r, "POST", endpoint, `{"action":"`+tc.action+`"}`, "", "", true)
		if code != 200 {
			t.Fatal(tc, code, string(b))
		}
		var ep anime.Episode
		if err := json.Unmarshal(b, &ep); err != nil || ep.State != tc.want {
			t.Fatal(ep, err)
		}
	}
	if err := r.store.SetMode(context.Background(), domain.ModeState{Mode: domain.ModeManualPause, Source: "test", SetAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	code, b := fileRequest(t, r, "POST", endpoint, `{"action":"pause"}`, "", "", true)
	if code != 200 {
		t.Fatal("pause should still stop work", code, string(b))
	}
	code, _ = fileRequest(t, r, "POST", endpoint, `{"action":"resume"}`, "", "", true)
	if code != 409 {
		t.Fatal("resume bypassed pause mode", code)
	}
	code, _ = fileRequest(t, r, "POST", endpoint, `{"action":"pause"}`, "", "", false)
	if code != 400 {
		t.Fatal("unconfirmed control", code)
	}
	code, _ = fileRequest(t, r, "POST", endpoint, `{"action":"pause","hash":"all"}`, "", "", true)
	if code != 400 {
		t.Fatal("client supplied hash", code)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 {
		t.Fatal(calls)
	}
}
