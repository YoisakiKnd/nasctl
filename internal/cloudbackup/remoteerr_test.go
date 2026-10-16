package cloudbackup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRemoteFailuresNameTheStepAndStatus(t *testing.T) {
	for _, mode := range []string{"direct", "encrypted"} {
		t.Run(mode, func(t *testing.T) {
			s, dav, _, _, _ := cloudRig(t)
			dav.mu.Lock()
			dav.failMove = true
			password := dav.password
			dav.mu.Unlock()
			_, e := s.Run(context.Background(), requestFor(s, mode), nil)
			if !errors.Is(e, ErrRemote) {
				t.Fatalf("want ErrRemote, got %v", e)
			}
			summary, detail, ok := Explain(e)
			if !ok || summary != "改为正式名称：MOVE 返回 HTTP 503" {
				t.Fatalf("summary %q ok %v", summary, ok)
			}
			for _, want := range []string{"步骤：改为正式名称（MOVE）", "HTTP 状态：503 Service Unavailable", "建议：服务器限流或繁忙"} {
				if !strings.Contains(detail, want) {
					t.Errorf("detail lacks %q:\n%s", want, detail)
				}
			}
			records := s.Settings().Records
			if len(records) == 0 || !strings.HasPrefix(records[0].Error, "WebDAV 改为正式名称：MOVE 返回 HTTP 503\n步骤：") {
				t.Fatalf("failed record does not carry the remote detail: %+v", records)
			}
			if strings.Contains(detail, password) || strings.Contains(e.Error(), password) {
				t.Fatal("credential leaked into the error")
			}
			if mode == "direct" && !strings.HasSuffix(strings.Split(strings.Split(detail, "远端路径：")[1], "\n")[0], "/") {
				t.Errorf("directory MOVE should use the collection form:\n%s", detail)
			}
		})
	}
}

func TestRemoteFailureExplainsRedirectsAndReplies(t *testing.T) {
	res := &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://user:secret@cdn.example/file?sig=abc"}}, Body: io.NopCloser(strings.NewReader("moved\x00 to\n\n the   CDN"))}
	e := failure("回读校验", "GET", "/dav/.x.part/a.txt", res, "")
	d := e.Detail()
	for _, want := range []string{"服务器要求跳转到：https://cdn.example/file", "服务器回复：moved to the CDN", "直链"} {
		if !strings.Contains(d, want) {
			t.Errorf("detail lacks %q:\n%s", want, d)
		}
	}
	if strings.Contains(d, "secret") || strings.Contains(d, "sig=") {
		t.Fatal("redirect credentials or signatures leaked")
	}
	if !errors.Is(e, ErrRemote) {
		t.Fatal("RemoteError must match ErrRemote")
	}
	long := printable(strings.Repeat("长", 400))
	if len([]rune(long)) != 301 {
		t.Fatalf("reply not bounded: %d", len([]rune(long)))
	}
}
