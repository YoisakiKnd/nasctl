package cloudbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func davURL(c Connection, rel string) string {
	u, _ := url.Parse(c.URL)
	if rel == "" {
		return u.String()
	}
	return u.JoinPath(rel).String()
}
func (s *Store) request(ctx context.Context, c Connection, method, rel string, body io.Reader, size int64, headers map[string]string) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, davURL(c, rel), body)
	if e != nil {
		return nil, ErrRemote
	}
	if body != nil {
		req.ContentLength = size
		if size == 0 {
			req.Body = http.NoBody
		}
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, e := s.client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrRemote
	}
	return res, nil
}
func finish(res *http.Response, allowed ...int) error {
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	for _, code := range allowed {
		if res.StatusCode == code {
			return nil
		}
	}
	return ErrRemote
}
func (s *Store) Probe(ctx context.Context, revision string) error {
	c, e := s.begin()
	if e != nil {
		return e
	}
	defer s.end()
	if c.Revision != revision || validateConnection(c) != nil {
		return ErrInvalid
	}
	body := strings.NewReader(`<d:propfind xmlns:d="DAV:"><d:prop><d:resourcetype/></d:prop></d:propfind>`)
	res, e := s.request(ctx, c, "PROPFIND", "", body, int64(body.Len()), map[string]string{"Depth": "0", "Content-Type": "application/xml; charset=utf-8"})
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 207 {
		return ErrRemote
	}
	bounded := &io.LimitedReader{R: res.Body, N: 65537}
	d := xml.NewDecoder(bounded)
	found := false
	for {
		token, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return ErrRemote
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Space == "DAV:" && start.Name.Local == "collection" {
			found = true
		}
	}
	if found && bounded.N > 0 {
		return nil
	}
	return ErrRemote
}
func (s *Store) mkdir(ctx context.Context, c Connection, rel string) error {
	res, e := s.request(ctx, c, "MKCOL", rel, nil, 0, nil)
	if e != nil {
		return e
	}
	return finish(res, 201)
}
func (s *Store) move(ctx context.Context, c Connection, from, to string) error {
	res, e := s.request(ctx, c, "MOVE", from, nil, 0, map[string]string{"Destination": davURL(c, to), "Overwrite": "F"})
	if e != nil {
		return e
	}
	return finish(res, 201, 204)
}
func (s *Store) removeTemporary(ctx context.Context, c Connection, rel string) {
	res, e := s.request(ctx, c, "DELETE", rel, nil, 0, nil)
	if e == nil {
		_ = finish(res, 200, 204, 404)
	}
}
func (s *Store) put(ctx context.Context, c Connection, rel string, r io.Reader, size int64, progress func(int64)) (string, error) {
	h := sha256.New()
	counter := &countReader{reader: io.TeeReader(r, h), progress: progress}
	res, e := s.request(ctx, c, "PUT", rel, counter, size, map[string]string{"If-None-Match": "*", "Content-Type": "application/octet-stream"})
	if e != nil {
		return "", e
	}
	if e = finish(res, 200, 201, 204); e != nil {
		return "", e
	}
	if counter.n != size {
		return "", ErrRemote
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if e = s.verify(ctx, c, rel, size, sum); e != nil {
		return "", e
	}
	return sum, nil
}
func (s *Store) verify(ctx context.Context, c Connection, rel string, size int64, sum string) error {
	res, e := s.request(ctx, c, "GET", rel, nil, 0, nil)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return ErrRemote
	}
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(res.Body, size+1), make([]byte, 64<<10))
	if e != nil || n != size || hex.EncodeToString(h.Sum(nil)) != sum {
		return ErrRemote
	}
	return nil
}

type countReader struct {
	reader   io.Reader
	n        int64
	progress func(int64)
}

func (r *countReader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	r.n += int64(n)
	if n > 0 && r.progress != nil {
		r.progress(r.n)
	}
	return n, e
}
