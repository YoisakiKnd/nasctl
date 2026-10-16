// A loopback-only DAV fixture for the compiled browser-DOM workflow. Never
// adopts existing data and never runs on a deployed NAS.
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

func main() {
	dir := flag.String("root", "", "new generated fixture directory")
	port := flag.Int("port", 18116, "loopback port")
	confirm := flag.Bool("fixture-only", false, "acknowledge generated data")
	flag.Parse()
	if !*confirm || *dir == "" {
		panic("--fixture-only --root required")
	}
	if entries, e := os.ReadDir(*dir); e == nil && len(entries) > 0 {
		panic("refusing nonempty fixture directory")
	}
	if e := os.MkdirAll(*dir, 0700); e != nil {
		panic(e)
	}
	os.WriteFile(path.Join(*dir, ".nasctl-webdav-fixture"), []byte("nasctl-webdav-fixture-v1\n"), 0600)
	root, e := os.OpenRoot(*dir)
	if e != nil {
		panic(e)
	}
	defer root.Close()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("fixture:generated-webdav-fixture-password")) {
			w.WriteHeader(401)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/dav/") {
			w.WriteHeader(404)
			return
		}
		rel := strings.TrimPrefix(r.URL.Path, "/dav/")
		rel = strings.TrimSuffix(rel, "/")
		if rel == "" {
			rel = "."
		}
		if path.Clean(rel) != rel || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
			w.WriteHeader(400)
			return
		}
		switch r.Method {
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(207)
			io.WriteString(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop></d:propstat></d:response></d:multistatus>`)
		case "MKCOL":
			if e := root.Mkdir(rel, 0700); e != nil {
				w.WriteHeader(405)
			} else {
				w.WriteHeader(201)
			}
		case "PUT":
			f, e := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				w.WriteHeader(412)
				return
			}
			n, e := io.Copy(f, io.LimitReader(r.Body, (16<<20)+1))
			f.Close()
			if e != nil || n > 16<<20 {
				root.Remove(rel)
				w.WriteHeader(413)
				return
			}
			w.WriteHeader(201)
		case "GET":
			f, e := root.Open(rel)
			if e != nil {
				w.WriteHeader(404)
				return
			}
			defer f.Close()
			i, e := f.Stat()
			if e != nil || !i.Mode().IsRegular() {
				w.WriteHeader(404)
				return
			}
			io.Copy(w, f)
		case "MOVE":
			u, e := url.Parse(r.Header.Get("Destination"))
			if e != nil || u.Host != r.Host || !strings.HasPrefix(u.Path, "/dav/") || r.Header.Get("Overwrite") != "F" {
				w.WriteHeader(400)
				return
			}
			to := strings.TrimPrefix(u.Path, "/dav/")
			if _, e = root.Lstat(to); e == nil {
				w.WriteHeader(412)
				return
			}
			if e = root.Rename(rel, to); e != nil {
				w.WriteHeader(409)
				return
			}
			w.WriteHeader(201)
		case "DELETE":
			if !strings.HasPrefix(rel, ".") || !strings.HasSuffix(rel, ".part") {
				w.WriteHeader(403)
				return
			}
			if e = root.RemoveAll(rel); e != nil {
				w.WriteHeader(500)
			} else {
				w.WriteHeader(204)
			}
		default:
			w.WriteHeader(405)
		}
	})
	fmt.Printf("Generated WebDAV fixture listening on 127.0.0.1:%d/dav/\n", *port)
	if e = http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *port), handler); e != nil {
		panic(e)
	}
}
