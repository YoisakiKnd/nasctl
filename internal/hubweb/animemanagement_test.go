package hubweb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/anime"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
)

func TestNativeSubscriptionQuickStateAndArchiveHTTP(t *testing.T) {
	r := nativeRig(t, nil)
	sub, err := r.srv.Anime.SaveSubscription(context.Background(), anime.Subscription{Title: "Managed", FeedURL: "https://example.com/rss", Season: 1, Enabled: true, AutoDownload: true, Include: "Managed.*", TMDBID: 42})
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/v1/anime/subscriptions/%d", sub.ID)
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":false,"archived":true}`, `{"enabled":false} {}`, `{"enabled":false,"extra":1}`} {
		code, _ := nativeRequest(t, r, "POST", base+"/enabled", body, "", "")
		if code != 400 {
			t.Fatal("bad state accepted", code, body)
		}
	}
	code, b := nativeRequest(t, r, "POST", base+"/enabled", `{"enabled":false}`, "", "")
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var paused anime.Subscription
	if err = json.Unmarshal(b, &paused); err != nil {
		t.Fatal(err)
	}
	if paused.Enabled || paused.Include != sub.Include || paused.TMDBID != 42 || !paused.AutoDownload {
		t.Fatal(paused)
	}
	code, b = nativeRequest(t, r, "POST", base+"/archive", `{"archived":true}`, "", "")
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var archived anime.Subscription
	json.Unmarshal(b, &archived)
	if archived.ArchivedAt == 0 || archived.Enabled {
		t.Fatal(archived)
	}
	code, _ = nativeRequest(t, r, "POST", base+"/enabled", `{"enabled":true}`, "", "")
	if code != 400 {
		t.Fatal("archived enabled", code)
	}
	code, b = nativeRequest(t, r, "POST", base+"/archive", `{"archived":false}`, "", "")
	if code != 200 {
		t.Fatal(code, string(b))
	}
	var restored anime.Subscription
	json.Unmarshal(b, &restored)
	if restored.ArchivedAt != 0 || restored.Enabled || !restored.AutoDownload {
		t.Fatal(restored)
	}
	code, _ = nativeRequest(t, r, "POST", base+"/enabled", `{"enabled":true}`, "", "")
	if code != 200 {
		t.Fatal(code)
	}
	code, _ = nativeRequest(t, r, "POST", "/api/v1/anime/subscriptions/999/archive", `{"archived":true}`, "", "")
	if code != 404 {
		t.Fatal(code)
	}
	audit, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, entry := range audit {
		if entry.Op == "anime.subscription.archive" {
			found[entry.Outcome] = true
		}
	}
	if !found["archived"] || !found["restored-paused"] {
		t.Fatal("archive audit missing", found)
	}
}
func TestNativeMetadataLookupErrorsAreSafeAndDoNotNeedWriteMode(t *testing.T) {
	r := nativeRig(t, func(_ *Config, j *jobs.Config) {
		if j != nil {
			j.ReadOnlyConsole = true
		}
	})
	for _, path := range []string{"/api/v1/anime/metadata/search?q=Managed", "/api/v1/anime/metadata/series/42"} {
		code, b := nativeRequest(t, r, "GET", path, "", "", "")
		if code != 400 || !strings.Contains(string(b), "TMDB") {
			t.Fatal(path, code, string(b))
		}
	}
	for _, path := range []string{"/api/v1/anime/subscriptions/1/archive", "/api/v1/anime/subscriptions/1/enabled"} {
		code, _ := nativeRequest(t, r, "POST", path, `{"archived":true}`, "", "")
		if code != 403 {
			t.Fatal("readonly state mutated", code)
		}
	}
}
