package config

import (
	"path/filepath"
	"strings"
)

// Album reads the local read-only index. Bounded processing re-executes the
// nasctl binary; WorkerBinary is an explicit override for tests and packaging.
//
// APIBase, Token and PublicURL belonged to the removed standalone album
// service. They are still decoded so that an old configuration fails with a
// migration message instead of an anonymous "unknown key".
type Album struct {
	Mode         string `toml:"mode"`
	WorkerBinary string `toml:"worker_binary"`
	APIBase      string `toml:"api_base"`
	Token        string `toml:"token"`
	PublicURL    string `toml:"public_url"`
}

func (a Album) EffectiveMode() string {
	if a.Mode != "" {
		return a.Mode
	}
	return "local"
}

func (c *Config) finalizeAlbum(fail func(string, ...any)) {
	a := &c.Hub.Album
	if a.WorkerBinary != "" && (!filepath.IsAbs(a.WorkerBinary) || strings.ContainsAny(a.WorkerBinary, "\r\n\x00")) {
		fail("hub.album.worker_binary must be an absolute executable path")
	}
	if a.Mode == "remote" || a.APIBase != "" || a.Token != "" || a.PublicURL != "" {
		fail("hub.album: the standalone album service was merged into nasctl; remove mode=\"remote\", api_base, token and public_url (the album index is read locally)")
		return
	}
	switch a.EffectiveMode() {
	case "local", "disabled":
	default:
		fail("hub.album.mode must be local or disabled")
	}
}
