package desktopapps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplicationLinksPersistAndRejectUnsafeSchemes(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	s := New(state)
	v, e := s.Get(ctx)
	if e != nil || len(v.Apps) != 0 {
		t.Fatal(v, e)
	}
	app := App{ID: "jellyfin", Name: "家庭影院", URL: "http://{host}:8096/", Icon: "play", Color: "violet"}
	saved, e := s.Save(ctx, v.Revision, []App{app})
	if e != nil || saved.Apps[0] != app {
		t.Fatal(saved, e)
	}
	if _, e = s.Save(ctx, v.Revision, nil); !errors.Is(e, ErrConflict) {
		t.Fatal("stale edit replaced links", e)
	}
	again, e := New(state).Get(ctx)
	if e != nil || again.Revision != saved.Revision {
		t.Fatal(again, e)
	}
	info, e := os.Stat(filepath.Join(state, "desktop-apps.json"))
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, e)
	}
	for _, raw := range []string{"https://example.com/?access_token=secret", "javascript:alert(1)", "data:text/html,x", "file:///etc/passwd", "//evil.example/", "https://user:secret@example.com/", "/\\evil.example/", "http://{invalid}:80/"} {
		a := app
		a.URL = raw
		if Validate(a) == nil {
			t.Fatal("unsafe link", raw)
		}
	}
	for _, raw := range []string{"https://example.com/app?q=1#home", "/jellyfin/", "http://[::1]:8096/"} {
		a := app
		a.URL = raw
		if e = Validate(a); e != nil {
			t.Fatal(raw, e)
		}
	}
}
