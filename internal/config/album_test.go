package config

import (
	"strings"
	"testing"
)

func TestAlbumModeAndRemovedRemoteService(t *testing.T) {
	for _, tc := range []struct {
		cfg  Album
		want string
	}{{Album{}, "local"}, {Album{Mode: "disabled"}, "disabled"}} {
		if tc.cfg.EffectiveMode() != tc.want {
			t.Fatal(tc)
		}
	}
	if _, err := Parse("[hub.album]\nmode=\"local\"\n[[hub.storage]]\nid=\"data\"\npath=\"/srv/data\"\n"); err != nil {
		t.Fatal("local album console", err)
	}
	for _, doc := range []string{"[hub.album]\nmode=\"unknown\"\n", "[hub.album]\nmode=\"remote\"\n", "[hub.album]\napi_base=\"http://127.0.0.1:8088\"\n", "[hub.album]\ntoken=\"orphan-token\"\n", "[hub.album]\npublic_url=\"https://nas.example/album/\"\n"} {
		_, err := Parse(doc)
		if err == nil {
			t.Fatal("invalid album config accepted", doc)
		}
		if strings.Contains(doc, "remote") || strings.Contains(doc, "api_base") {
			if !strings.Contains(err.Error(), "merged into nasctl") {
				t.Fatal("remote album config without migration hint", err)
			}
		}
	}
}
