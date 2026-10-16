package cloudbackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
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
		return nil, networkFailure(method, rel, e)
	}
	return res, nil
}

// finish accepts one of the allowed statuses and otherwise explains the
// failure for the given step, always draining and closing the body.
func finish(step string, res *http.Response, allowed ...int) error {
	defer res.Body.Close()
	for _, code := range allowed {
		if res.StatusCode == code {
			_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
			return nil
		}
	}
	e := failure(step, res.Request.Method, relOf(res), res, "")
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8192))
	return e
}

// relOf recovers the remote path of a response for error details.
func relOf(res *http.Response) string {
	if res.Request == nil || res.Request.URL == nil {
		return ""
	}
	return res.Request.URL.Path
}

// collection marks a remote path as a directory. nginx's WebDAV module
// refuses MKCOL, and MOVE or DELETE of a directory, without the trailing
// slash; other servers accept either form.
func collection(rel string) string { return strings.TrimSuffix(rel, "/") + "/" }
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
		return failure("连接测试", "PROPFIND", "", res, "")
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
			return &RemoteError{Step: "连接测试", Method: "PROPFIND", Status: 207, Reason: "服务器的目录列表不是有效的 WebDAV XML"}
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Space == "DAV:" && start.Name.Local == "collection" {
			found = true
		}
	}
	if found && bounded.N > 0 {
		return nil
	}
	return &RemoteError{Step: "连接测试", Method: "PROPFIND", Status: 207, Reason: "地址指向的不是一个目录，或目录列表超过 64 KiB"}
}
func (s *Store) mkdir(ctx context.Context, c Connection, rel string) error {
	res, e := s.request(ctx, c, "MKCOL", collection(rel), nil, 0, nil)
	if e != nil {
		return e
	}
	return finish("建立远端目录", res, 201)
}

// move publishes a finished upload under its final name. Directories use the
// collection form on both sides.
func (s *Store) move(ctx context.Context, c Connection, from, to string, directory bool) error {
	if directory {
		from, to = collection(from), collection(to)
	}
	res, e := s.request(ctx, c, "MOVE", from, nil, 0, map[string]string{"Destination": davURL(c, to), "Overwrite": "F"})
	if e != nil {
		return e
	}
	return finish("改为正式名称", res, 201, 204)
}
func (s *Store) removeTemporary(ctx context.Context, c Connection, rel string, directory bool) {
	if directory {
		rel = collection(rel)
	}
	res, e := s.request(ctx, c, "DELETE", rel, nil, 0, nil)
	if e == nil {
		_ = finish("清理临时文件", res, 200, 204, 404)
	}
}
func (s *Store) put(ctx context.Context, c Connection, rel string, r io.Reader, size int64, progress func(int64)) (string, error) {
	h := sha256.New()
	counter := &countReader{reader: io.TeeReader(r, h), progress: progress}
	res, e := s.request(ctx, c, "PUT", rel, counter, size, map[string]string{"If-None-Match": "*", "Content-Type": "application/octet-stream"})
	if e != nil {
		return "", e
	}
	if e = finish("上传", res, 200, 201, 204); e != nil {
		return "", e
	}
	if counter.n != size {
		return "", &RemoteError{Step: "上传", Method: "PUT", Path: rel, Reason: fmt.Sprintf("只发送了 %d 字节，应为 %d 字节；本地文件在上传中发生了变化", counter.n, size)}
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
		return failure("回读校验", "GET", rel, res, "")
	}
	h := sha256.New()
	n, e := io.CopyBuffer(h, io.LimitReader(res.Body, size+1), make([]byte, 64<<10))
	switch {
	case e != nil:
		return &RemoteError{Step: "回读校验", Method: "GET", Path: rel, Status: 200, Reason: "读回时连接中断（" + printable(e.Error()) + "）"}
	case n != size:
		return &RemoteError{Step: "回读校验", Method: "GET", Path: rel, Status: 200, Reason: fmt.Sprintf("读回 %d 字节，上传的是 %d 字节。服务端可能改写了文件，或仍在后台转存", n, size)}
	case hex.EncodeToString(h.Sum(nil)) != sum:
		return &RemoteError{Step: "回读校验", Method: "GET", Path: rel, Status: 200, Reason: "读回的内容与上传的不一致（SHA-256 不同）。服务端可能改写了文件内容"}
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
