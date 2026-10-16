package hubweb

import (
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/YoisakiKnd/nasctl/internal/album"
	"github.com/YoisakiKnd/nasctl/internal/files"
)

// Private album directories stay where they are on disk. Until the vault is
// unlocked in this browser session the console treats them as absent: they
// are left out of listings, searches and the trash, and any request that names
// a path inside one is refused. Unlocking never allows moving, renaming,
// deleting, copying or zipping a folder that is or holds a private directory,
// because that would silently take the contents out of protection.

var (
	errPrivateLocked = errors.New("private folder is locked")
	errPrivateWhole  = errors.New("private folder cannot be handled as a whole")
)

// privateUse is what a request does with a path.
type privateUse int

const (
	// useInside reads or writes at the path: listing, opening, uploading, or
	// creating something there.
	useInside privateUse = iota
	// useCopy takes the path's contents somewhere else: copying or zipping.
	// The private folder itself may be copied out once unlocked.
	useCopy
	// useMove relocates or removes the path: rename, move or trash.
	useMove
)

type privateGate struct {
	s        *Server
	r        *http.Request
	folders  []album.Folder
	unlocked *bool
}

func (s *Server) privateGate(r *http.Request) *privateGate {
	g := &privateGate{s: s, r: r}
	if s.AlbumControl != nil {
		g.folders = s.AlbumControl.PrivateFolders()
	}
	return g
}

// privateSession reports whether the request carries an unlocked vault
// session, and keeps that session alive while it is being used.
func (s *Server) privateSession(r *http.Request) bool {
	if s.PhotoVault == nil || !principalOf(r.Context()).DeviceAdmin() {
		return false
	}
	token, owner := s.vaultIdentity(r)
	return s.PhotoVault.Extend(token, owner)
}

func (g *privateGate) open() bool {
	if g.unlocked == nil {
		v := g.s.privateSession(g.r)
		g.unlocked = &v
	}
	return *g.unlocked
}

func (g *privateGate) any(root string) bool {
	for _, f := range g.folders {
		if f.Root == root {
			return true
		}
	}
	return false
}

func cleanStoragePath(p string) string {
	c := strings.Trim(path.Clean("/"+p), "/")
	if c == "." {
		return ""
	}
	return c
}

// within reports whether p is folder or lies beneath it ("" is the root).
func within(p, folder string) bool {
	return folder == "" || p == folder || strings.HasPrefix(p, folder+"/")
}

// folderOf returns the private folder containing p, if any.
func (g *privateGate) folderOf(root, p string) (string, bool) {
	for _, f := range g.folders {
		if f.Root == root && within(p, f.Path) {
			return f.Path, true
		}
	}
	return "", false
}

// hidden reports whether an entry at p is left out of what this session sees.
// It is a display filter; requests are judged by check.
func (g *privateGate) hidden(root, p string) bool {
	if len(g.folders) == 0 {
		return false
	}
	_, inside := g.folderOf(root, cleanStoragePath(p))
	return inside && !g.open()
}

// isFolder reports whether p is itself a private directory.
func (g *privateGate) isFolder(root, p string) bool {
	p = cleanStoragePath(p)
	for _, f := range g.folders {
		if f.Root == root && f.Path == p {
			return true
		}
	}
	return false
}

// check judges a path both as spelled and as it resolves through links, so a
// link elsewhere in the root cannot reach into a private directory.
func (g *privateGate) check(root, p string, use privateUse) error {
	if len(g.folders) == 0 || !g.any(root) {
		return nil
	}
	candidates := []string{cleanStoragePath(p)}
	if g.s.Files != nil {
		if real, err := g.s.Files.Resolve(root, candidates[0]); err == nil {
			if real = cleanStoragePath(real); real != candidates[0] {
				candidates = append(candidates, real)
			}
		}
	}
	for _, c := range candidates {
		for _, f := range g.folders {
			if f.Root != root {
				continue
			}
			holds := c != f.Path && within(f.Path, c)
			if use != useInside && holds || use == useMove && c == f.Path {
				return errPrivateWhole
			}
			if within(c, f.Path) && !g.open() {
				return errPrivateLocked
			}
		}
	}
	return nil
}

// trashItem judges a trash record by where it was deleted from. A record whose
// origin is unknown counts as private while the root has private directories.
func (g *privateGate) trashItem(root string, item files.TrashItem) error {
	if !g.any(root) || g.open() {
		return nil
	}
	if item.RecoveryRequired {
		return errPrivateLocked
	}
	if _, inside := g.folderOf(root, cleanStoragePath(item.OriginalPath)); inside {
		return errPrivateLocked
	}
	return nil
}

func (g *privateGate) trashID(root, id string) error {
	if !g.any(root) || g.open() {
		return nil
	}
	item, err := g.s.Files.TrashEntry(root, id)
	if err != nil {
		// The operation itself reports a missing or invalid record.
		return nil
	}
	return g.trashItem(root, item)
}

// privateError writes the refusal for a gate decision and reports whether
// one was written.
func privateError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errPrivateWhole):
		writeError(w, 409, "private_folder", "这里是私密目录或包含私密目录，不能整体移动、重命名、删除、复制或打包下载。\n如需这样做，请先在“相册管理”中取消该目录的私密设置。")
	default:
		writeError(w, 403, "vault_locked", "这是私密目录，请先解锁保险箱。\n私密目录只对设备管理员开放，并且需要保险箱密码。")
	}
	return true
}
