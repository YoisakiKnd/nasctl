package config

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Album uses the local read-only index by default. An explicitly configured
// APIBase retains compatibility with a separate album service.
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
	if a.APIBase != "" {
		return "remote"
	}
	return "local"
}

func ValidateAlbumBase(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("album API base must be an HTTP(S) URL without credentials, query or fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate())) {
		return fmt.Errorf("album API base must use localhost or a literal loopback/private IP; publish a container port on loopback")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("album API port is invalid")
		}
	}
	if u.RawPath != "" || strings.Contains(u.Path, "\\") || (u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) {
		return fmt.Errorf("album API path must be clean and unescaped")
	}
	return nil
}
func (c *Config) finalizeAlbum(fail func(string, ...any)) {
	a := &c.Hub.Album
	if a.WorkerBinary != "" && (!filepath.IsAbs(a.WorkerBinary) || strings.ContainsAny(a.WorkerBinary, "\r\n\x00")) {
		fail("hub.album.worker_binary must be an absolute executable path")
	}
	switch a.EffectiveMode() {
	case "local", "disabled":
		if a.APIBase != "" || a.Token != "" || a.PublicURL != "" {
			fail("hub.album: local/disabled mode cannot include remote connection settings")
		}
		return
	case "remote":
	default:
		fail("hub.album.mode must be local, remote, or disabled")
		return
	}
	if a.APIBase == "" {
		fail("hub.album.api_base is required in remote mode")
		return
	}
	if err := ValidateAlbumBase(a.APIBase); err != nil {
		fail("hub.album.api_base: %v", err)
	}
	if len(a.Token) > 4096 || strings.ContainsAny(a.Token, "\r\n\x00") {
		fail("hub.album.token is too long or contains control characters")
	}
	if a.PublicURL != "" {
		u, err := url.Parse(a.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			fail("hub.album.public_url must be an HTTP(S) URL without credentials, query or fragment")
		}
	}
}
