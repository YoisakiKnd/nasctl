package anime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const sourceSearchHTML = `<ul class="an-ul"><li><a href="/Home/Bangumi/123"><span data-src="/images/Bangumi/test.jpg"></span><div class="an-text" title="番剧 &amp; 第二季">番剧</div></a></li><li><a href="https://evil.invalid/Home/Bangumi/456"><div class="an-text" title="伪造">伪造</div></a></li><li><a href="/Home/Bangumi/123"><div class="an-text" title="重复">重复</div></a></li></ul>`
const sourceDetailsHTML = `<p class="bangumi-title">番剧 第二季 <a href="/RSS/Bangumi?bangumiId=123">RSS</a></p><a href="https://bgm.tv/subject/42">资料</a><div class="subgroup-text" id="7"><a href="/Home/PublishGroup/7">字幕组 &amp; Friends</a><a href="/RSS/Bangumi?bangumiId=123&amp;subgroupid=7">RSS</a></div><div class="subgroup-text" id="8">其他组<a href="https://evil.invalid/RSS/Bangumi?bangumiId=123&amp;subgroupid=8">fake</a></div>`

func TestSourceDiscoveryIsBoundedFixedOriginAndReadOnly(t *testing.T) {
	e, down, cfg := newEngine(t, sampleRSS)
	_ = cfg
	e.cfg.TMDBToken = "must-not-leak"
	before, _ := e.Store.Subscriptions(context.Background())
	calls := 0
	e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || !strings.Contains(r.Header.Get("User-Agent"), "nasctl/") {
			t.Fatal("unexpected credentials or agent")
		}
		switch r.URL.Host + r.URL.Path {
		case "api.bgm.tv/v0/search/subjects":
			var body struct {
				Keyword string `json:"keyword"`
				Filter  struct {
					Type []int `json:"type"`
				} `json:"filter"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Keyword != "番剧 & 名称" || !reflect.DeepEqual(body.Filter.Type, []int{2}) || r.Method != "POST" || r.URL.Query().Get("offset") != "12" {
				t.Fatal("wrong Bangumi query", body, r.URL)
			}
			return metadataResponse(r, 200, `{"total":50,"data":[{"id":42,"type":2,"name":"Original","name_cn":"番剧","summary":"<script>text</script>","images":{"common":"https://lain.bgm.tv/pic/test.jpg"}},{"id":43,"type":1,"name":"book"},{"id":42,"type":2,"name":"duplicate"}]}`), nil
		case "mikanani.me/Home/Search":
			if r.URL.Query().Get("searchstr") != "番剧 & 名称" {
				t.Fatal(r.URL)
			}
			return metadataResponse(r, 200, sourceSearchHTML), nil
		case "mikanani.me/Home/Bangumi/123":
			return metadataResponse(r, 200, sourceDetailsHTML), nil
		default:
			t.Fatal("unexpected origin", r.URL)
			return nil, nil
		}
	})}
	bg, err := e.SearchSources(context.Background(), "bangumi", "番剧 & 名称", 2)
	if err != nil || len(bg.Results) != 1 || bg.Results[0].Name != "番剧" || !bg.HasMore {
		t.Fatal(bg, err)
	}
	mk, err := e.SearchSources(context.Background(), "mikan", "番剧 & 名称", 1)
	if err != nil || len(mk.Results) != 1 || mk.Results[0].ID != 123 || mk.Results[0].Name != "番剧 & 第二季" {
		t.Fatal(mk, err)
	}
	details, err := e.MikanSource(context.Background(), 123)
	if err != nil || len(details.Feeds) != 2 || details.BangumiID != 42 || details.Feeds[0].Name != "字幕组 & Friends" || details.Feeds[0].URL != "https://mikanani.me/RSS/Bangumi?bangumiId=123&subgroupid=7" {
		t.Fatal(details, err)
	}
	after, _ := e.Store.Subscriptions(context.Background())
	if calls != 3 || down.adds != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("discovery wrote state or downloader")
	}
	for _, args := range []struct {
		p, q string
		n    int
	}{{"bad", "title", 1}, {"mikan", "title", 2}, {"bangumi", "", 1}, {"bangumi", "bad\nquery", 1}, {"bangumi", strings.Repeat("字", 101), 1}, {"bangumi", "title", 11}} {
		if _, err = e.SearchSources(context.Background(), args.p, args.q, args.n); err == nil {
			t.Fatal("bad input accepted", args)
		}
	}
	if calls != 3 {
		t.Fatal("invalid input reached network")
	}
}
func TestSourceFailuresAndUntrustedHTML(t *testing.T) {
	r := strings.NewReader(sourceSearchHTML + strings.Repeat("unneeded release history", 300000))
	prefix, err := readMikanSearchPrefix(r)
	if err != nil || !strings.Contains(string(prefix), "an-ul") || r.Len() < 4000000 {
		t.Fatal("search buffered release history", len(prefix), r.Len(), err)
	}
	if sourcePoster("https://lain.bgm.tv/r/400/pic/cover/l/test.jpg", "lain.bgm.tv", "/pic/") == "" {
		t.Fatal("Bangumi resized cover rejected")
	}
	if _, err := parseMikanSearch([]byte("<html>challenge</html>")); err == nil {
		t.Fatal("challenge accepted")
	}
	if _, err := parseMikanSource([]byte(strings.ReplaceAll(sourceDetailsHTML, "bangumiId=123", "bangumiId=124")), 123); err == nil {
		t.Fatal("wrong feed ID accepted")
	}
	for _, raw := range []string{"http://lain.bgm.tv/pic/a.jpg", "https://evil.invalid/pic/a.jpg", "//evil.invalid/pic/a.jpg", "https://user:pw@lain.bgm.tv/pic/a.jpg"} {
		if sourcePoster(raw, "lain.bgm.tv", "/pic/") != "" {
			t.Fatal("bad poster", raw)
		}
	}
	many := `<ul class="an-ul">`
	for n := 1; n <= 30; n++ {
		many += fmt.Sprintf(`<a href="/Home/Bangumi/%d"><div class="an-text" title="Test"></div></a>`, n)
	}
	out, err := parseMikanSearch([]byte(many))
	if err != nil || len(out.Results) != 12 || !out.Truncated {
		t.Fatal(out, err)
	}
	e, _, _ := newEngine(t, sampleRSS)
	for _, scenario := range []string{"redirect", "large", "bad-json", "rate"} {
		t.Run(scenario, func(t *testing.T) {
			e.feedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch scenario {
				case "redirect":
					v := metadataResponse(r, 302, "")
					v.Header.Set("Location", "https://evil.invalid")
					return v, nil
				case "large":
					return metadataResponse(r, 200, strings.Repeat("x", maxMetadataBytes+1)), nil
				case "rate":
					return metadataResponse(r, 429, "secret diagnostic"), nil
				default:
					return metadataResponse(r, 200, "{bad}"), nil
				}
			})}
			if _, err := e.SearchSources(context.Background(), "bangumi", "test", 1); err == nil || strings.Contains(err.Error(), "secret diagnostic") {
				t.Fatal(err)
			}
		})
	}
}
