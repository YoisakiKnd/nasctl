package hubweb

import (
	"encoding/json"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/photovault"
)

type privateControlFake struct{ albumControlFake }

func (*privateControlFake) PrivateFolders() []album.Folder {
	return []album.Folder{{Root: "media", Path: "sub"}}
}
func (*privateControlFake) Configuration() album.Configuration {
	return album.Configuration{Directories: []album.Directory{{Code: "secret", Name: "Secret", Root: "media", Path: "sub", Enabled: true, Private: true, Managed: true}}}
}

func TestPrivateFoldersStayHiddenUntilTheVaultIsUnlocked(t *testing.T) {
	r := writeRig(t, nil)
	vault, err := photovault.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	r.srv.PhotoVault = vault
	r.srv.VaultMover = vaultNoopMover{}
	r.srv.AlbumControl = &privateControlFake{}
	t.Cleanup(r.srv.WaitWork)
	jar, _ := cookiejar.New(nil)
	r.client.Jar = jar
	if err := os.Symlink("sub", filepath.Join(r.root, "alias")); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string) (int, string) {
		t.Helper()
		code, b := fileRequest(t, r, method, path, body, "", "", true)
		return code, string(b)
	}
	expect := func(want int, method, path, body string) string {
		t.Helper()
		code, b := call(method, path, body)
		if code != want {
			t.Fatalf("%s %s: got %d, want %d: %s", method, path, code, want, b)
		}
		return b
	}
	listing := func(path string) []entryDoc {
		t.Helper()
		var out listingResponse
		if err := json.Unmarshal([]byte(expect(200, "GET", "/api/v1/files/list?root=media&path="+path, "")), &out); err != nil {
			t.Fatal(err)
		}
		return out.Entries
	}
	names := func(entries []entryDoc) string {
		var out []string
		for _, e := range entries {
			if e.Private {
				out = append(out, e.Name+"*")
			} else {
				out = append(out, e.Name)
			}
		}
		return strings.Join(out, ",")
	}

	// Locked: the folder is absent from listings and searches, and everything
	// that names it, including through a link, is refused.
	if got := names(listing("")); strings.Contains(got, "sub") {
		t.Fatal("locked listing shows the private folder:", got)
	}
	expect(403, "GET", "/api/v1/files/list?root=media&path=sub", "")
	expect(403, "GET", "/api/v1/files/stat?root=media&path=sub/inner.txt", "")
	expect(403, "GET", "/api/v1/files/download?root=media&path=sub/inner.txt", "")
	expect(403, "GET", "/api/v1/files/download?root=media&path=alias/inner.txt", "")
	expect(403, "GET", "/api/v1/files/preview?root=media&path=/sub/inner.txt", "")
	if b := expect(200, "GET", "/api/v1/files/search?root=media&q=inner&depth=3", ""); strings.Contains(b, "inner.txt") {
		t.Fatal("locked search found a private file:", b)
	}
	expect(403, "POST", "/api/v1/files/mkdir", `{"root":"media","path":"sub/new"}`)
	expect(403, "PUT", "/api/v1/files/upload?root=media&path=sub/up.txt", "x")
	expect(409, "POST", "/api/v1/files/rename", `{"root":"media","from":"sub","to":"moved"}`)
	expect(409, "POST", "/api/v1/files/trash", `{"root":"media","path":"sub"}`)
	expect(409, "GET", "/api/v1/files/archive?root=media&path=/", "")
	expect(403, "GET", "/api/v1/files/archive?root=media&path=sub", "")
	expect(403, "POST", "/api/v1/files/copy", `{"root":"media","to_root":"media","from":"notes.txt","to":"sub/notes.txt","limit":100,"max_bytes":1000000}`)
	expect(403, "POST", "/api/v1/files/batch", `{"action":"trash","items":[{"root":"media","path":"sub/inner.txt"}]}`)
	expect(403, "POST", "/api/v1/album/directories", `{"code":"secret","name":"Secret","root":"media","path":"sub","enabled":true}`)

	expect(200, "POST", "/api/v1/album/vault/create", `{"password":"disposable test password"}`)
	expect(200, "POST", "/api/v1/album/vault/unlock", `{"password":"disposable test password"}`)

	// Unlocked: the folder opens and is marked, but it still cannot be moved,
	// renamed, removed or swept up with its parent.
	if got := names(listing("")); !strings.Contains(got, "sub*") {
		t.Fatal("unlocked listing does not mark the private folder:", got)
	}
	if got := names(listing("sub")); got != "inner.txt" {
		t.Fatal("unlocked private listing:", got)
	}
	if b := expect(200, "GET", "/api/v1/files/download?root=media&path=alias/inner.txt", ""); b != "inner" {
		t.Fatal("unlocked download:", b)
	}
	expect(409, "POST", "/api/v1/files/rename", `{"root":"media","from":"sub","to":"moved"}`)
	expect(409, "POST", "/api/v1/files/rename", `{"root":"media","from":"alias","to":"moved"}`)
	expect(409, "GET", "/api/v1/files/archive?root=media&path=/", "")
	expect(200, "GET", "/api/v1/files/archive?root=media&path=sub", "")
	expect(200, "POST", "/api/v1/files/trash", `{"root":"media","path":"sub/inner.txt"}`)
	var trash struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(expect(200, "GET", "/api/v1/files/trash?root=media", "")), &trash); err != nil || len(trash.Items) != 1 {
		t.Fatal("unlocked trash", err, trash)
	}

	// Locked again: the trashed private file is hidden and cannot be restored.
	expect(200, "POST", "/api/v1/album/vault/lock", "")
	if b := expect(200, "GET", "/api/v1/files/trash?root=media", ""); strings.Contains(b, trash.Items[0].ID) {
		t.Fatal("locked trash shows a private record:", b)
	}
	expect(403, "POST", "/api/v1/files/restore", `{"root":"media","id":"`+trash.Items[0].ID+`"}`)
	expect(403, "GET", "/api/v1/files/list?root=media&path=sub", "")
}
