package config

import "testing"

func TestAlbumConnectionValidationAndAlbumOnlyConsole(t *testing.T) {
	for _, tc := range []struct {
		cfg  Album
		want string
	}{{Album{}, "local"}, {Album{APIBase: "http://127.0.0.1:8088"}, "remote"}, {Album{Mode: "disabled"}, "disabled"}} {
		if tc.cfg.EffectiveMode() != tc.want {
			t.Fatal(tc)
		}
	}
	for _, value := range []string{"http://127.0.0.1:8088", "https://localhost:8088/gallery", "http://[::1]:8088/", "http://192.168.1.3:8088", "http://10.0.0.2:8088"} {
		if err := ValidateAlbumBase(value); err != nil {
			t.Fatal(value, err)
		}
	}
	for _, value := range []string{"https://example.com", "http://8.8.8.8", "http://album-container:8088", "file:///tmp/state", "http://user:pass@127.0.0.1", "http://127.0.0.1?q=secret", "http://127.0.0.1/#secret", "http://127.0.0.1:99999", "http://127.0.0.1/a/../b", "http://127.0.0.1/%2fapi"} {
		if err := ValidateAlbumBase(value); err == nil {
			t.Fatal("unsafe album address", value)
		}
	}
	if _, err := Parse("[hub.album]\napi_base=\"http://127.0.0.1:8088\"\npublic_url=\"https://nas.example/album/\"\n"); err != nil {
		t.Fatal("album-only console", err)
	}
	for _, doc := range []string{"[hub.album]\nmode=\"unknown\"\n", "[hub.album]\nmode=\"local\"\napi_base=\"http://127.0.0.1:8088\"\n", "[hub.album]\nmode=\"remote\"\n", "[hub.album]\ntoken=\"orphan-token\"\n", "[hub.album]\napi_base=\"http://127.0.0.1:8088\"\npublic_url=\"javascript:alert(1)\"\n", "[hub.album]\napi_base=\"http://127.0.0.1:8088\"\ntoken=\"bad\\nheader\"\n"} {
		if _, err := Parse(doc); err == nil {
			t.Fatal("invalid album config accepted", doc)
		}
	}
}
