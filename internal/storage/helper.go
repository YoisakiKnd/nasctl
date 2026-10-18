package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

// HelperSocket is where systemd listens for the storage helper. Only the
// nasctl service user may connect; each connection starts one short-lived
// helper process as root, which serves a single request and exits.
const HelperSocket = "/run/nasctl/storage.sock"

// Share is one Samba share.
type Share struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	Guest    bool   `json:"guest"`
}

// NetMount is a network share mounted under MountBase.
type NetMount struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"` // smb or nfs
	Server   string `json:"server"`
	Share    string `json:"share"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"` // write-only: never returned
	ReadOnly bool   `json:"read_only"`
	Persist  bool   `json:"persist"`
	Mounted  bool   `json:"mounted"`
}

// Request is everything the console can ask of the helper.
type Request struct {
	Op       string    `json:"op"`
	Device   string    `json:"device,omitempty"`
	Name     string    `json:"name,omitempty"`
	Label    string    `json:"label,omitempty"`
	Confirm  string    `json:"confirm,omitempty"`
	Persist  bool      `json:"persist,omitempty"`
	Forget   bool      `json:"forget,omitempty"`
	Enabled  bool      `json:"enabled,omitempty"`
	Shares   []Share   `json:"shares,omitempty"`
	Password string    `json:"password,omitempty"`
	Net      *NetMount `json:"net,omitempty"`
}

// Response carries a result or a reason.
type Response struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Detail string          `json:"detail,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Status is the helper's view of the machine's storage tooling and settings.
type Status struct {
	Tools     map[string]bool `json:"tools"`
	Automount bool            `json:"automount"`
	Shares    []Share         `json:"shares"`
	NetMounts []NetMount      `json:"net_mounts"`
	ShareUser bool            `json:"share_user"` // the Samba account has a password
	Persisted []string        `json:"persisted"`  // mount points mounted at boot
}

// SMART is a disk's health summary from smartctl.
type SMART struct {
	Available    bool   `json:"available"`
	Passed       *bool  `json:"passed"`
	TemperatureC *int   `json:"temperature_c"`
	PowerOnHours *int   `json:"power_on_hours"`
	Detail       string `json:"detail,omitempty"`
	Reallocated  *int   `json:"reallocated_sectors"`
	Pending      *int   `json:"pending_sectors"`
	PercentUsed  *int   `json:"percent_used"`
	Model        string `json:"model,omitempty"`
}

// ErrHelperMissing means the helper's socket is not installed.
var ErrHelperMissing = errors.New("storage: the storage helper is not installed")

// HelperError is a refusal or failure the helper explained.
type HelperError struct{ Message, Detail string }

func (e *HelperError) Error() string { return e.Message }

// Client talks to the helper socket.
type Client struct{ Socket string }

// Available reports whether the helper socket exists.
func (c Client) Available() bool {
	info, err := os.Stat(c.socket())
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func (c Client) socket() string {
	if c.Socket != "" {
		return c.Socket
	}
	return HelperSocket
}

// Call sends one request. Formatting a large disk can take minutes, so the
// deadline follows the context.
func (c Client) Call(ctx context.Context, req Request, out any) error {
	if !c.Available() {
		return ErrHelperMissing
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket())
	if err != nil {
		return ErrHelperMissing
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	var res Response
	if err := json.NewDecoder(bufio.NewReader(&limitReader{conn, 4 << 20})).Decode(&res); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("storage: the helper gave no answer")
	}
	if !res.OK {
		return &HelperError{Message: res.Error, Detail: res.Detail}
	}
	if out != nil && len(res.Data) > 0 {
		return json.Unmarshal(res.Data, out)
	}
	return nil
}

type limitReader struct {
	conn net.Conn
	n    int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errors.New("storage: helper answer too large")
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.conn.Read(p)
	l.n -= int64(n)
	return n, err
}
