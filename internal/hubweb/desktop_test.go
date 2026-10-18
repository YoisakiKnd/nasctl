package hubweb

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/desktopapps"
)

func TestSystemStatusAndSharedWallpaper(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Wallpaper = desktopapps.New(t.TempDir())

	code, body := fileRequest(t, r, "GET", "/api/v1/system/status", "", "", "", false)
	var status struct {
		CPU struct {
			Cores int `json:"cores"`
		} `json:"cpu"`
		Memory struct {
			TotalBytes int64 `json:"total_bytes"`
		} `json:"memory"`
		Network []any `json:"network"`
	}
	if code != 200 || json.Unmarshal(body, &status) != nil || status.CPU.Cores < 1 || status.Memory.TotalBytes <= 0 || status.Network == nil {
		t.Fatal(code, string(body))
	}

	if code, _ := fileRequest(t, r, "GET", "/api/v1/desktop/wallpaper", "", "", "", false); code != 404 {
		t.Fatal("no wallpaper yet", code)
	}
	if code, _ := fileRequest(t, r, "PUT", "/api/v1/desktop/wallpaper", "not an image", "", "", true); code != 400 {
		t.Fatal("non-image accepted", code)
	}
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)
	if code, _ := fileRequest(t, r, "PUT", "/api/v1/desktop/wallpaper", png, "", "", false); code != 400 {
		t.Fatal("unconfirmed upload", code)
	}
	if code, _ := fileRequest(t, r, "PUT", "/api/v1/desktop/wallpaper", png, "", "", true); code != 200 {
		t.Fatal("upload", code)
	}
	if code, got := fileRequest(t, r, "GET", "/api/v1/desktop/wallpaper", "", "", "", false); code != 200 || string(got) != png {
		t.Fatal("read back", code)
	}
	if code, _ := fileRequest(t, r, "DELETE", "/api/v1/desktop/wallpaper", "", "", "", true); code != 200 {
		t.Fatal("delete", code)
	}
	if code, _ := fileRequest(t, r, "GET", "/api/v1/desktop/wallpaper", "", "", "", false); code != 404 {
		t.Fatal("still there after delete", code)
	}
}
