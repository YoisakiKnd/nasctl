package cloudbackup

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RemoteError explains which WebDAV exchange failed and what the server said,
// so an operator can tell a missing permission from a refused MOVE or a
// redirecting gateway. It matches ErrRemote. It never carries credentials:
// they travel only in the Authorization header, and the configured URL has no
// user information.
type RemoteError struct {
	Step     string // what nasctl was doing, in Chinese
	Method   string
	Path     string // remote path relative to the configured URL
	Status   int    // 0 when no response arrived
	Location string // redirect target, when the server asked for one
	Reply    string // the start of the server's reply, printable text only
	Reason   string // what nasctl observed when the status alone is not the cause
}

func (e *RemoteError) Error() string        { return ErrRemote.Error() + ": " + e.Summary() }
func (e *RemoteError) Is(target error) bool { return target == ErrRemote }

// Summary is one line naming the step and outcome.
func (e *RemoteError) Summary() string {
	switch {
	case e.Status != 0:
		return fmt.Sprintf("%s：%s 返回 HTTP %d", e.Step, e.Method, e.Status)
	case e.Reason != "":
		return e.Step + "：" + e.Reason
	default:
		return e.Step + "失败"
	}
}

// Detail is a short multi-line explanation for the expandable error view.
func (e *RemoteError) Detail() string {
	var b strings.Builder
	line := func(label, value string) {
		if value != "" {
			b.WriteString(label + "：" + value + "\n")
		}
	}
	line("步骤", e.Step+"（"+e.Method+"）")
	line("远端路径", "/"+strings.TrimPrefix(e.Path, "/"))
	if e.Status != 0 {
		line("HTTP 状态", fmt.Sprintf("%d %s", e.Status, http.StatusText(e.Status)))
	}
	line("服务器要求跳转到", e.Location)
	line("原因", e.Reason)
	line("服务器回复", e.Reply)
	line("建议", e.hint())
	return strings.TrimRight(b.String(), "\n")
}

func (e *RemoteError) hint() string {
	switch {
	case e.Status >= 300 && e.Status < 400:
		if e.Method == http.MethodGet {
			return "下载被跳转了 5 次以上、跳转到了带账号信息的地址，或从 https 跳转到 http（未允许 HTTP 时）。nasctl 只跟随最多 5 次指向 https 的下载跳转，请检查 WebDAV 服务的直链设置。"
		}
		return "上传、建目录等写操作不跟随跳转，以免文件被发到别处。请把 WebDAV 地址改成跳转后的地址（注意 http/https 和末尾的 /）。"
	case e.Status == 401:
		return "账号或密码不正确，请在连接设置中重新填写。"
	case e.Status == 403:
		return "这个账号没有该目录的写入权限，请在 WebDAV 服务端授予读写权限。"
	case e.Status == 404 && e.Method == http.MethodGet:
		return "文件刚上传后读不到。服务端可能在后台异步上传到网盘，请改用直接存储文件的 WebDAV 目录。"
	case e.Status == 404 || e.Status == 409:
		return "上级目录不存在，或目标已存在。请确认 WebDAV 地址指向一个已存在的目录。"
	case e.Status == 405 || e.Status == 501:
		return "服务器不支持 " + e.Method + "。nasctl 需要 PROPFIND、MKCOL、PUT、GET、MOVE 和 DELETE。"
	case e.Status == 412:
		return "服务器拒绝了“禁止覆盖”条件（If-None-Match: *），或目标已经存在。"
	case e.Status == 413:
		return "文件超过服务端或反向代理的大小上限（例如 nginx 的 client_max_body_size，Cloudflare 免费版 100 MB）。"
	case e.Status == 423:
		return "远端文件或目录被锁定，请稍后重试。"
	case e.Status == 429 || e.Status == 503:
		return "服务器限流或繁忙，请稍后重试，或降低备份频率。"
	case e.Status == 507:
		return "远端空间不足。"
	case e.Status >= 500:
		return "WebDAV 服务端出错，请查看服务端日志。"
	case e.Status == 0 && strings.Contains(e.Reason, "awaiting response headers"):
		return "服务器 30 秒内没有响应。服务端处理太慢（转存网盘、杀毒、慢速硬盘）时会出现，请换用更快的存储。"
	case e.Status == 0 && strings.Contains(e.Reason, "certificate"):
		return "HTTPS 证书不受信任。请使用受信任的证书，或在可信局域网中启用 HTTP。"
	case e.Status == 0 && e.Reason != "" && strings.Contains(e.Reason, "dial"):
		return "无法连接到服务器，请检查地址、端口和网络。"
	}
	return ""
}

// failure builds a RemoteError from a response, reading at most a short,
// printable prefix of the reply. The caller still closes the body.
func failure(step, method, rel string, res *http.Response, reason string) *RemoteError {
	e := &RemoteError{Step: step, Method: method, Path: rel, Reason: reason}
	if res == nil {
		return e
	}
	e.Status = res.StatusCode
	if loc := res.Header.Get("Location"); loc != "" {
		if u, err := url.Parse(loc); err == nil {
			u.User = nil
			u.RawQuery, u.Fragment = "", ""
			e.Location = u.String()
		}
	}
	if res.StatusCode >= 300 && res.Body != nil {
		buf := make([]byte, 512)
		n, _ := io.ReadFull(res.Body, buf)
		e.Reply = printable(string(buf[:n]))
	}
	return e
}

// networkFailure describes a request that got no response. The URL is cut out
// of the error text: it is already in the detail as a relative path.
func networkFailure(method, rel string, err error) *RemoteError {
	cause := err
	var ue *url.Error
	if errors.As(err, &ue) {
		cause = ue.Err
	}
	return &RemoteError{Step: stepFor(method), Method: method, Path: rel, Reason: "没有收到响应（" + printable(cause.Error()) + "）"}
}

func stepFor(method string) string {
	switch method {
	case "PROPFIND":
		return "连接测试"
	case "MKCOL":
		return "建立远端目录"
	case http.MethodPut:
		return "上传"
	case http.MethodGet:
		return "读取远端文件"
	case "MOVE":
		return "改为正式名称"
	case http.MethodDelete:
		return "清理临时文件"
	}
	return "WebDAV 请求"
}

// printable keeps a reply readable and bounded: control characters become
// spaces, runs of space collapse, and the text is cut at 300 runes.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s
}

// Explain returns the one-line summary and multi-line detail of a remote
// failure, or false when err carries no remote detail.
func Explain(err error) (summary, detail string, ok bool) {
	var re *RemoteError
	if !errors.As(err, &re) {
		return "", "", false
	}
	return re.Summary(), re.Detail(), true
}

// recordError is what a failed backup record shows: the remote step and its
// detail when the failure came from the WebDAV server, the fallback otherwise.
// It stays under 2 KiB so 128 records fit the history file's budget.
func recordError(fallback string, err error) string {
	summary, detail, ok := Explain(err)
	if !ok {
		return fallback
	}
	text := "WebDAV " + summary + "\n" + detail
	if len(text) > 2000 {
		cut := 2000
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}
