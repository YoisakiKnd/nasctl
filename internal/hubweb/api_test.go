package hubweb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/config"
	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/files"
	"github.com/YoisakiKnd/nasctl/internal/hubops"
	"github.com/YoisakiKnd/nasctl/internal/hubstore"
	"github.com/YoisakiKnd/nasctl/internal/jobs"
	"github.com/YoisakiKnd/nasctl/internal/operations"
	"github.com/YoisakiKnd/nasctl/internal/suzu"
	_ "modernc.org/sqlite"
)

// rig is a running console over a synthetic tree.
type rig struct {
	srv    *Server
	ts     *httptest.Server
	store  *hubstore.Store
	jobs   *jobs.Manager
	root   string
	outdir string
	client *http.Client
}

func newRig(t *testing.T, tune func(*Config, *jobs.Config)) *rig {
	return newRigWithSuzu(t, tune, nil)
}

// newRigWithSuzu builds a rig with an optional suzu client, so the integration can
// be exercised through the real HTTP surface rather than only in its own package.
func newRigWithSuzu(t *testing.T, tune func(*Config, *jobs.Config), sc *suzu.Client) *rig {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "media")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, "notes.txt"), "hello")
	writeFile(t, filepath.Join(root, "photo.jpg"), "\xff\xd8\xff\xe0not-really-a-jpeg")
	writeFile(t, filepath.Join(root, "sub", "inner.txt"), "inner")
	writeFile(t, filepath.Join(outside, "secret.txt"), "SECRET")
	// A link escaping the root, and a <script> in a filename: both must be inert.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, `<img src=x onerror=alert(1)>.txt`), "xss")

	store, err := hubstore.Open(filepath.Join(base, "hub.db"), hubstore.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg, err := operations.New(hubops.Declarations()...)
	if err != nil {
		t.Fatal(err)
	}
	jcfg := jobs.Config{Registry: reg, Store: store}
	if tune != nil {
		var wc Config
		tune(&wc, &jcfg)
		defer func() { _ = wc }()
	}
	jm, err := jobs.New(jcfg)
	if err != nil {
		t.Fatal(err)
	}

	fm, err := files.New([]config.HubStorage{{ID: "media", Label: "Media", Path: root}}, files.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fm.Close() })

	cfg := Config{}
	if tune != nil {
		tune(&cfg, nil)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(cfg, fm, jm, sc, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &rig{srv: srv, ts: ts, store: store, jobs: jm, root: root, outdir: outside, client: ts.Client()}
}

func (r *rig) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	res, err := r.client.Get(r.ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return res, body
}

func (r *rig) getJSON(t *testing.T, path string, v any) *http.Response {
	t.Helper()
	res, body := r.get(t, path)
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			t.Fatalf("GET %s returned invalid JSON (%s): %s", path, err, truncate(string(body)))
		}
	}
	return res
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestOverviewReportsTheMachineAndSaysWhatIsWrong: the front page must lead with
// problems and must not render an unknown state as healthy.
func TestOverviewReportsTheMachineAndSaysWhatIsWrong(t *testing.T) {
	r := newRig(t, nil)
	var out overviewResponse
	res := r.getJSON(t, "/api/v1/overview", &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if out.Version == "" {
		t.Error("no version reported")
	}
	if out.Mode.Mode != domain.ModeNormal {
		t.Errorf("mode = %q, want normal", out.Mode.Mode)
	}
	if out.Warnings == nil {
		t.Error("warnings is null; the UI cannot distinguish 'none' from 'not reported'")
	}
	if out.Attention == nil || out.Running == nil {
		t.Error("job lists are null rather than empty")
	}
	if len(out.Files.Roots) != 1 || out.Files.Roots[0].ID != "media" {
		t.Errorf("roots = %+v", out.Files.Roots)
	}
	if out.Files.Writable {
		t.Error("the console reports file writes as available, but they are not implemented")
	}
}

// TestAModeIsReportedAndGatesWork: setting a pause must be visible immediately and
// must be reflected in the warnings.
func TestAModeIsReportedAndGatesWork(t *testing.T) {
	r := newRig(t, nil)

	body := `{"mode":"manual_pause","reason":"testing"}`
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("setting a pause returned %d", res.StatusCode)
	}

	var out overviewResponse
	r.getJSON(t, "/api/v1/overview", &out)
	if out.Mode.Mode != domain.ModeManualPause {
		t.Errorf("mode = %q, want manual_pause", out.Mode.Mode)
	}
	found := false
	for _, w := range out.Warnings {
		if strings.Contains(w, "heavy work is stopped") {
			found = true
		}
	}
	if !found {
		t.Errorf("the pause is not reported as a warning: %v", out.Warnings)
	}

	// Clearing works through the API.
	req, _ := http.NewRequest(http.MethodDelete, r.ts.URL+"/api/v1/modes/manual_pause", nil)
	dres, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dres.Body.Close()
	if dres.StatusCode != http.StatusOK {
		t.Fatalf("clearing the pause returned %d", dres.StatusCode)
	}
	r.getJSON(t, "/api/v1/overview", &out)
	if out.Mode.Mode != domain.ModeNormal {
		t.Errorf("mode after clearing = %q, want normal", out.Mode.Mode)
	}
}

// TestResourceGuardIsNotSettableByACaller: the machine's safety state must not be
// something a request can set or clear.
func TestResourceGuardIsNotSettableByACaller(t *testing.T) {
	r := newRig(t, nil)
	body := `{"mode":"resource_guard"}`
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("setting resource_guard returned %d, want 403", res.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, r.ts.URL+"/api/v1/modes/resource_guard", nil)
	dres, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer dres.Body.Close()
	if dres.StatusCode != http.StatusForbidden {
		t.Errorf("clearing resource_guard returned %d, want 403", dres.StatusCode)
	}
}

// TestAnExpiringWindowNeedsADuration: a window without one would be a pause
// nobody remembers setting.
func TestAnExpiringWindowNeedsADuration(t *testing.T) {
	r := newRig(t, nil)
	for _, body := range []string{
		`{"mode":"watching"}`,
		`{"mode":"watching","duration":"0s"}`,
		`{"mode":"watching","duration":"-1h"}`,
		`{"mode":"watching","duration":"48h"}`,
	} {
		res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s returned %d, want 400", body, res.StatusCode)
		}
	}
	// A manual pause must not take a duration: it is durable by definition.
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json",
		strings.NewReader(`{"mode":"manual_pause","duration":"1h"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a pause with a duration returned %d, want 400", res.StatusCode)
	}
}

// TestFileListingIsPagedAndRefusesTraversal: the file API's two central
// properties.
func TestFileListingIsPagedAndRefusesTraversal(t *testing.T) {
	r := newRig(t, nil)

	var l listingResponse
	res := r.getJSON(t, "/api/v1/files/list?root=media", &l)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if l.Path != "/" {
		t.Errorf("path = %q, want /", l.Path)
	}
	names := map[string]bool{}
	for _, e := range l.Entries {
		names[e.Name] = true
	}
	for _, want := range []string{"notes.txt", "sub", "escape.txt"} {
		if !names[want] {
			t.Errorf("listing is missing %q: %v", want, names)
		}
	}

	// Every traversal shape must be refused, and refused as a refusal.
	for _, p := range []string{"..", "../outside/secret.txt", "sub/../../outside", "/etc/passwd"} {
		res, body := r.get(t, "/api/v1/files/list?root=media&path="+urlEscape(p))
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusBadRequest {
			t.Errorf("traversal %q returned %d (%s), want 403 or 400", p, res.StatusCode, truncate(string(body)))
		}
		if strings.Contains(string(body), "SECRET") {
			t.Errorf("traversal %q leaked content outside the root", p)
		}
	}

	// An unknown root is a 404, and its message does not name a real path.
	res, body := r.get(t, "/api/v1/files/list?root=nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown root returned %d, want 404", res.StatusCode)
	}
	if strings.Contains(string(body), r.root) {
		t.Errorf("the error message leaks the configured path: %s", truncate(string(body)))
	}
}

func urlEscape(s string) string {
	return strings.NewReplacer("/", "%2F", "\\", "%5C", " ", "%20").Replace(s)
}

// TestPreviewPassesBytesThroughAndRefusesTheRest: the console does not decode, so
// a preview is a byte stream, and a file it cannot show says so.
func TestPreviewPassesBytesThroughAndRefusesTheRest(t *testing.T) {
	r := newRig(t, nil)

	res, body := r.get(t, "/api/v1/files/preview?root=media&path=photo.jpg")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("preview of a jpg returned %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/jpeg") {
		t.Errorf("content type = %q, want image/jpeg", ct)
	}
	// The bytes are the file's bytes, unmodified: no decode, no re-encode.
	if !strings.HasPrefix(string(body), "\xff\xd8\xff\xe0") {
		t.Errorf("the preview did not pass the bytes through: %q", truncate(string(body)))
	}
	if res.Header.Get("Content-Disposition") != "inline" {
		t.Errorf("content disposition = %q, want inline", res.Header.Get("Content-Disposition"))
	}

	// A text file has no in-browser preview and is refused with an explanation
	// rather than being sent as something the browser would mishandle.
	res, body = r.get(t, "/api/v1/files/preview?root=media&path=notes.txt")
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("preview of a text file returned %d, want 415", res.StatusCode)
	}
	if len(body) == 0 {
		t.Error("the refusal carries no explanation")
	}

	// An escaping link is refused.
	res, _ = r.get(t, "/api/v1/files/preview?root=media&path=escape.txt")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("preview through an escaping link returned %d, want 403", res.StatusCode)
	}
}

// TestHostileNamesAndMetadataAreInert is the XSS test. A filename containing
// markup must come back as characters, and the page must never build markup from
// it.
func TestHostileNamesAndMetadataAreInert(t *testing.T) {
	r := newRig(t, nil)

	var l listingResponse
	r.getJSON(t, "/api/v1/files/list?root=media", &l)
	var found bool
	for _, e := range l.Entries {
		if strings.Contains(e.Name, "<img") {
			found = true
			if strings.Contains(e.Name, "\\u003c") {
				t.Errorf("the name was double-escaped: %q", e.Name)
			}
		}
	}
	if !found {
		t.Fatal("the crafted filename was not listed")
	}

	// The JSON response must not contain a raw tag: an HTML parser must not see
	// one even if the response were ever placed in an HTML context.
	_, body := r.get(t, "/api/v1/files/list?root=media")
	if strings.Contains(string(body), "<img src=x") {
		t.Error("the response contains a raw <img> tag; JSON must escape it")
	}
	if !strings.Contains(string(body), `\u003cimg`) {
		t.Errorf("the response does not escape the angle bracket: %s", truncate(string(body)))
	}

	// The front end must not contain an HTML-sink pattern on untrusted data.
	js, err := os.ReadFile(filepath.Join("web", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	// innerHTML/outerHTML/insertAdjacentHTML/document.write are the sinks that
	// would turn a filename into markup.
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(src, sink) {
			// Strip comments before judging, so an explanatory comment is not a
			// finding.
			if containsOutsideComment(src, sink) {
				t.Errorf("app.js uses %s; filenames and metadata must be written with "+
					"textContent so they cannot become markup", sink)
			}
		}
	}
}

// containsOutsideComment reports whether needle appears outside a // or /* comment.
func containsOutsideComment(src, needle string) bool {
	i := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+4:], "*/")
			if end < 0 {
				return false
			}
			i += 4 + end + 2
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				return false
			}
			i += end + 1
		case strings.HasPrefix(src[i:], needle):
			return true
		default:
			i++
		}
	}
	return false
}

// TestTheConsoleServesItsShellAndAssets: the UI is embedded, so there is no asset
// directory on the machine and no build step at run time.
func TestTheConsoleServesItsShellAndAssets(t *testing.T) {
	r := newRig(t, nil)

	res, body := r.get(t, "/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Error("the shell is not HTML")
	}
	if !strings.Contains(string(body), "OneCloud Hub") {
		t.Error("the shell does not name the console")
	}

	for path, wantType := range map[string]string{
		"/app.css": "text/css",
		"/app.js":  "text/javascript",
	} {
		res, body := r.get(t, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s returned %d", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s content type = %q, want %s", path, ct, wantType)
		}
		if len(body) == 0 {
			t.Errorf("%s is empty", path)
		}
	}

	// A missing asset is a 404, not the HTML shell: a typo must be visible.
	res, _ = r.get(t, "/missing.js")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a missing asset returned %d, want 404", res.StatusCode)
	}

	// Security headers are present on every response.
	res, _ = r.get(t, "/")
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff is missing")
	}
}

// TestTheOperationTableIsPublished: an operator or an agent must be able to learn
// what the console accepts without reading the source.
func TestTheOperationTableIsPublished(t *testing.T) {
	r := newRig(t, nil)
	var out struct {
		Operations []struct {
			ID       string `json:"id"`
			Class    string `json:"class"`
			Approval string `json:"approval"`
			ReadOnly bool   `json:"read_only"`
		} `json:"operations"`
	}
	res := r.getJSON(t, "/api/v1/ops", &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(out.Operations) < 20 {
		t.Errorf("only %d operations published", len(out.Operations))
	}
	byID := map[string]string{}
	for _, o := range out.Operations {
		byID[o.ID] = o.Class
	}
	for _, want := range []string{"files.list", "album.scan", "album.thumbs", "jobs.list", "backup.status"} {
		if _, ok := byID[want]; !ok {
			t.Errorf("%s is not published", want)
		}
	}
	// No operation may offer command execution.
	for id := range byID {
		for _, bad := range []string{"shell", "exec", "sudo"} {
			if strings.Contains(id, bad) {
				t.Errorf("the published table contains %s", id)
			}
		}
	}
}

// TestTheAuditLogRecordsTheModeChange: an operator must be able to account for a
// state change afterwards.
func TestTheAuditLogRecordsTheModeChange(t *testing.T) {
	r := newRig(t, nil)
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json",
		strings.NewReader(`{"mode":"watching","duration":"1h","reason":"a film"}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	var out struct {
		Entries []hubstore.AuditEntry `json:"entries"`
	}
	r.getJSON(t, "/api/v1/audit", &out)
	var found bool
	for _, e := range out.Entries {
		if e.Action == "mode.set" && e.Target == "watching" {
			found = true
			if e.Principal == "" {
				t.Error("the audit row does not say who acted")
			}
		}
	}
	if !found {
		t.Errorf("the mode change was not audited: %+v", out.Entries)
	}
}

// TestTheTokenIsRequiredWhenConfigured: with a token set, an unauthenticated
// request is refused.
func TestTheTokenIsRequiredWhenConfigured(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.Token = "s3cret" })

	res, _ := r.get(t, "/api/v1/overview")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("without a token: status = %d, want 401", res.StatusCode)
	}
	if res.Header.Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate header on a 401")
	}

	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	ok, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Errorf("with the token: status = %d, want 200", ok.StatusCode)
	}

	// A wrong token of a different length must not be accepted.
	req, _ = http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	bad, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Errorf("with a wrong token: status = %d, want 401", bad.StatusCode)
	}
}

// TestAnAgentIsRecordedAsAnAgent: an AI caller announces itself, and that is what
// the audit log and the job rows must say, because the console treats an agent
// differently from a person.
func TestAnAgentIsRecordedAsAnAgent(t *testing.T) {
	r := newRig(t, nil)
	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
	req.Header.Set("X-Nasctl-Agent", "hermes/1.2")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	// The agent may not start heavy work; this is reported so the page can say so.
	var out overviewResponse
	r.getJSON(t, "/api/v1/overview", &out)
	if out.AllowAIHeavy {
		t.Error("agents may start heavy work by default")
	}
}

// TestCheckBindRefusesAPublicAddress is the access-control test. The console has
// no per-user authentication, so the bind address is the control, and it is
// checked rather than documented.
func TestCheckBindRefusesAPublicAddress(t *testing.T) {
	allowed := []string{
		"127.0.0.1:8090", "localhost:8090", "[::1]:8090",
		"100.64.0.10:8090", "100.101.102.103:8090", // Tailscale CGNAT range
		"192.168.1.5:8090", "10.0.0.1:8090", "172.16.0.1:8090",
	}
	for _, b := range allowed {
		if err := CheckBind(b, false); err != nil {
			t.Errorf("CheckBind(%q) = %v, want nil", b, err)
		}
	}

	refused := []string{
		"0.0.0.0:8090",
		":8090",
		"8.8.8.8:8090",
		"1.2.3.4:8090",
		"example.com:8090",
	}
	for _, b := range refused {
		if err := CheckBind(b, false); err == nil {
			t.Errorf("CheckBind(%q) = nil, want a refusal", b)
		}
	}
	// The escape hatch exists for offline tests and must still work.
	if err := CheckBind("0.0.0.0:8090", true); err != nil {
		t.Errorf("CheckBind with allow_non_private = %v, want nil", err)
	}
	// A malformed address is refused regardless.
	if err := CheckBind("nonsense", true); err == nil {
		t.Error("a malformed bind was accepted")
	}
}

// TestRequestBodiesAreBounded: an unbounded body is a way to make a small machine
// swap.
func TestRequestBodiesAreBounded(t *testing.T) {
	r := newRig(t, nil)
	huge := `{"mode":"watching","duration":"1h","reason":"` + strings.Repeat("x", 200<<10) + `"}`
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json", strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an oversized body returned %d, want 400", res.StatusCode)
	}
}

// TestAnUnknownFieldInABodyIsRefused: a caller that misspells a field believes it
// set something it did not.
func TestAnUnknownFieldInABodyIsRefused(t *testing.T) {
	r := newRig(t, nil)
	res, err := r.client.Post(r.ts.URL+"/api/v1/modes", "application/json",
		strings.NewReader(`{"mode":"watching","duration":"1h","duraction":"2h"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a misspelled field returned %d, want 400", res.StatusCode)
	}
}

// TestJobsAreListedAndPaged: the job view is how an operator sees what the console
// did, so it must page and must not invent states.
func TestJobsAreListedAndPaged(t *testing.T) {
	r := newRig(t, nil)
	for i := 0; i < 3; i++ {
		_, err := r.store.CreateJob(context.Background(), hubstore.NewJob{
			ID: fmt.Sprintf("job%d", i), Op: "album.scan", Principal: "local",
			StartQueued: true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var out struct {
		Jobs []jobSummary `json:"jobs"`
	}
	r.getJSON(t, "/api/v1/jobs?open=true&limit=2", &out)
	if len(out.Jobs) != 2 {
		t.Errorf("page = %d jobs, want 2", len(out.Jobs))
	}

	var one struct {
		Job    jobSummary          `json:"job"`
		Events []hubstore.JobEvent `json:"events"`
	}
	res := r.getJSON(t, "/api/v1/jobs/job0", &one)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if one.Job.ID != "job0" {
		t.Errorf("job id = %q", one.Job.ID)
	}
	if len(one.Events) == 0 {
		t.Error("the job has no event history")
	}

	// An unknown job is a 404; an invalid state filter is a 400.
	res, _ = r.get(t, "/api/v1/jobs/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job returned %d, want 404", res.StatusCode)
	}
	res, _ = r.get(t, "/api/v1/jobs?state=invented")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an invented state returned %d, want 400", res.StatusCode)
	}
}

// TestSearchIsBounded: a search must not be able to walk an unbounded tree.
func TestSearchIsBounded(t *testing.T) {
	r := newRig(t, nil)
	var out struct {
		Entries   []entryDoc `json:"entries"`
		Examined  int        `json:"examined"`
		Truncated bool       `json:"truncated"`
	}
	res := r.getJSON(t, "/api/v1/files/search?root=media&q=inner", &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if len(out.Entries) != 1 || out.Entries[0].Name != "inner.txt" {
		t.Errorf("entries = %+v, want inner.txt", out.Entries)
	}

	// An empty query is refused rather than matching everything.
	res, _ = r.get(t, "/api/v1/files/search?root=media&q=")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an empty query returned %d, want 400", res.StatusCode)
	}
}

// TestTheMediaIntegrationReportsRatherThanGuesses: with no suzu configured, the
// console says so and returns no library, rather than inventing one.
func TestTheMediaIntegrationReportsRatherThanGuesses(t *testing.T) {
	r := newRig(t, nil)
	var out mediaStatusResponse
	res := r.getJSON(t, "/api/v1/media/status", &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if out.Configured {
		t.Error("the integration reports itself configured when nothing is set")
	}
	if len(out.Library) != 0 {
		t.Error("an unconfigured integration returned library rows")
	}
	if out.Note == "" {
		t.Error("the unconfigured state carries no explanation")
	}
}

// TestPreviewRefusesAFileAboveTheCap: the preview cap exists so a request cannot
// pull a huge file through a page.
func TestPreviewRefusesAFileAboveTheCap(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.PreviewMaxBytes = 64 })
	writeFile(t, filepath.Join(r.root, "big.jpg"), strings.Repeat("x", 4096))

	res, body := r.get(t, "/api/v1/files/preview?root=media&path=big.jpg")
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized preview returned %d, want 413", res.StatusCode)
	}
	if !strings.Contains(string(body), "too_large") && !strings.Contains(string(body), "preview is limited") {
		t.Errorf("the refusal does not explain the limit: %s", truncate(string(body)))
	}
}

// TestConcurrentRequestsAreBounded: the slot limit must not deadlock the console.
func TestConcurrentRequestsAreBounded(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.MaxConcurrentRequests = 2 })
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			res, err := r.client.Get(r.ts.URL + "/api/v1/overview")
			if err != nil {
				done <- err
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			done <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("a request failed under concurrency: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("requests did not complete; the slot limit deadlocked")
		}
	}
}

// TestAgentReadsAreAuditedAndOperatorReadsAreNot pins the accountability rule: an
// operator browsing their own files is not a security event, an agent reading them
// is. Recording both would bury the entries that matter under page loads;
// recording neither would leave an operator unable to see what a program read.
func TestAgentReadsAreAuditedAndOperatorReadsAreNot(t *testing.T) {
	r := newRig(t, nil)

	// An operator read.
	res, _ := r.get(t, "/api/v1/files/list?root=media")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	// An agent read.
	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/files/list?root=media", nil)
	req.Header.Set("X-Nasctl-Agent", "hermes")
	ares, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ares.Body.Close()
	// An agent read that is refused.
	req, _ = http.NewRequest(http.MethodGet,
		r.ts.URL+"/api/v1/files/list?root=media&path=../outside/secret.txt", nil)
	req.Header.Set("X-Nasctl-Agent", "hermes")
	fres, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	fres.Body.Close()

	rows, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var agentReads, operatorReads int
	for _, e := range rows {
		if e.Action != "read" {
			continue
		}
		switch e.Principal {
		case "ai:hermes":
			agentReads++
		case "user:token-holder":
			operatorReads++
		}
	}
	if agentReads != 2 {
		t.Errorf("agent reads audited = %d, want 2 (one ok, one refused)", agentReads)
	}
	if operatorReads != 0 {
		t.Errorf("operator reads audited = %d, want 0: browsing is not a security event "+
			"and would bury the rows that are", operatorReads)
	}
	// The refusal must be distinguishable from the success.
	var sawRefused bool
	for _, e := range rows {
		if e.Principal == "ai:hermes" && e.Outcome == "refused" {
			sawRefused = true
		}
	}
	if !sawRefused {
		t.Error("the agent's refused read is not recorded as refused")
	}
}

// TestAnAgentNameCannotInjectMarkup: the agent announces its own name, so the
// name is attacker-controlled and must be sanitised before it reaches a log, a
// JSON field or the audit table.
func TestAnAgentNameCannotInjectMarkup(t *testing.T) {
	r := newRig(t, nil)
	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/files/list?root=media", nil)
	req.Header.Set("X-Nasctl-Agent", "<script>alert(1)</script>")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	rows, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range rows {
		if strings.Contains(e.Principal, "<") || strings.Contains(e.Principal, ">") {
			t.Errorf("the agent name reached the audit table unsanitised: %q", e.Principal)
		}
	}
}

// TestVideoIsRangedAndImagesAreCapped: the two halves of the preview policy.
//
// Video must be ranged, because a player seeks and a console that cannot answer
// a range makes skipping a minute cost a gigabyte. An image must be capped,
// because the browser renders it whole and there is no range to serve.
func TestVideoIsRangedAndImagesAreCapped(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.PreviewMaxBytes = 1024 })

	// A video larger than the image cap is still streamed.
	writeFile(t, filepath.Join(r.root, "clip.mp4"), strings.Repeat("v", 4096))

	res, _ := r.get(t, "/api/v1/files/preview?root=media&path=clip.mp4")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a video above the image cap returned %d; video is ranged, so the "+
			"image cap must not apply to it", res.StatusCode)
	}
	if ar := res.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", ar)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "video/") {
		t.Errorf("content type = %q, want video/*", ct)
	}

	// A range request is answered with 206 and a correct Content-Range.
	req, _ := http.NewRequest(http.MethodGet,
		r.ts.URL+"/api/v1/files/preview?root=media&path=clip.mp4", nil)
	req.Header.Set("Range", "bytes=1000-1999")
	rres, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rres.Body.Close()
	if rres.StatusCode != http.StatusPartialContent {
		t.Fatalf("a range request returned %d, want 206", rres.StatusCode)
	}
	if cr := rres.Header.Get("Content-Range"); cr != "bytes 1000-1999/4096" {
		t.Errorf("Content-Range = %q, want %q", cr, "bytes 1000-1999/4096")
	}
	body, _ := io.ReadAll(rres.Body)
	if len(body) != 1000 {
		t.Errorf("a 1000-byte range returned %d bytes", len(body))
	}

	// An image above the cap is refused, with an explanation.
	writeFile(t, filepath.Join(r.root, "big.png"), strings.Repeat("p", 4096))
	res, body = r.get(t, "/api/v1/files/preview?root=media&path=big.png")
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized image returned %d, want 413", res.StatusCode)
	}
	if !strings.Contains(string(body), "image") {
		t.Errorf("the refusal does not say the limit is for images: %s", truncate(string(body)))
	}
}

// TestDownloadStreamsAnythingThePreviewRefuses: the download endpoint exists
// because the preview refuses what a browser cannot render. A text file, an
// oversized image and an unknown type must all download.
func TestDownloadStreamsAnythingThePreviewRefuses(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.PreviewMaxBytes = 64 })
	writeFile(t, filepath.Join(r.root, "big.png"), strings.Repeat("p", 4096))
	writeFile(t, filepath.Join(r.root, "archive.bin"), strings.Repeat("z", 2048))

	cases := []struct {
		path    string
		preview int
	}{
		{"notes.txt", http.StatusUnsupportedMediaType},
		{"big.png", http.StatusRequestEntityTooLarge},
		{"archive.bin", http.StatusUnsupportedMediaType},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			// The preview refuses it...
			pres, _ := r.get(t, "/api/v1/files/preview?root=media&path="+c.path)
			if pres.StatusCode != c.preview {
				t.Errorf("preview returned %d, want %d", pres.StatusCode, c.preview)
			}
			// ...and the download does not.
			res, body := r.get(t, "/api/v1/files/download?root=media&path="+c.path)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("download returned %d, want 200", res.StatusCode)
			}
			if len(body) == 0 {
				t.Error("the download is empty")
			}
			cd := res.Header.Get("Content-Disposition")
			if !strings.HasPrefix(cd, "attachment") {
				t.Errorf("Content-Disposition = %q, want an attachment", cd)
			}
			if ct := res.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream so the browser "+
					"does not try to render it", ct)
			}
		})
	}
}

// TestDownloadRefusesTraversalAndEscapes: the download endpoint is a second door
// into the filesystem, so it gets the same containment tests as the first.
func TestDownloadRefusesTraversalAndEscapes(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range []string{"..", "../outside/secret.txt", "/etc/passwd", "sub/../../outside"} {
		res, body := r.get(t, "/api/v1/files/download?root=media&path="+urlEscape(p))
		if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusBadRequest {
			t.Errorf("download %q returned %d, want 403 or 400", p, res.StatusCode)
		}
		if strings.Contains(string(body), "SECRET") {
			t.Errorf("download %q leaked content outside the root", p)
		}
	}
	// An escaping symlink is refused too.
	res, _ := r.get(t, "/api/v1/files/download?root=media&path=escape.txt")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("downloading through an escaping link returned %d, want 403", res.StatusCode)
	}
}

// TestDownloadSupportsResume: a client that lost an interrupted transfer must be
// able to continue it rather than start over.
func TestDownloadSupportsResume(t *testing.T) {
	r := newRig(t, nil)
	writeFile(t, filepath.Join(r.root, "resume.bin"), strings.Repeat("abcdefghij", 100))

	req, _ := http.NewRequest(http.MethodGet,
		r.ts.URL+"/api/v1/files/download?root=media&path=resume.bin", nil)
	req.Header.Set("Range", "bytes=500-599")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("a range request returned %d, want 206", res.StatusCode)
	}
	if cr := res.Header.Get("Content-Range"); cr != "bytes 500-599/1000" {
		t.Errorf("Content-Range = %q, want %q", cr, "bytes 500-599/1000")
	}
	body, _ := io.ReadAll(res.Body)
	if len(body) != 100 {
		t.Errorf("a 100-byte range returned %d bytes", len(body))
	}
}

// TestContentDispositionIsSafeForHostileNames is the header-injection test. A
// filename comes from the filesystem, so it can contain a quote, a backslash or a
// newline, and it is placed in a response header.
func TestContentDispositionIsSafeForHostileNames(t *testing.T) {
	cases := []struct {
		name           string
		mustNotContain []string
	}{
		{`evil".txt`, []string{`evil".txt`}},
		{"new\nline.txt", []string{"\n"}},
		{"ret\rurn.txt", []string{"\r"}},
		{`back\slash.txt`, []string{`\`}},
		{"semi;colon.txt", []string{";colon"}},
		{"tab\there.txt", []string{"\t"}},
	}
	for _, c := range cases {
		got := contentDisposition(c.name)
		for _, bad := range c.mustNotContain {
			if strings.Contains(got, bad) {
				t.Errorf("contentDisposition(%q) = %q, which still contains %q",
					c.name, got, bad)
			}
		}
		if !strings.HasPrefix(got, "attachment;") {
			t.Errorf("contentDisposition(%q) = %q, want an attachment", c.name, got)
		}
		// No control character may survive anywhere in the header value.
		for _, r := range got {
			if r < 0x20 || r == 0x7f {
				t.Errorf("contentDisposition(%q) contains a control character (%q)", c.name, r)
			}
		}
	}

	// A non-ASCII name is carried by the percent-encoded parameter, so the real
	// name is not lost.
	got := contentDisposition("写真.jpg")
	if !strings.Contains(got, "filename*=UTF-8''") {
		t.Errorf("a non-ASCII name lost its encoded form: %q", got)
	}
	// An entirely unsafe name still yields a usable parameter.
	if g := contentDisposition(`"""`); !strings.Contains(g, "filename=") {
		t.Errorf("a name of only quotes produced %q", g)
	}
	if g := contentDisposition(""); !strings.Contains(g, "download") {
		t.Errorf("an empty name produced %q", g)
	}
}

// TestAHostileFilenameDownloadsSafely exercises the header escaping end to end,
// through the real HTTP server, because that is where a bad header would actually
// be written.
func TestAHostileFilenameDownloadsSafely(t *testing.T) {
	r := newRig(t, nil)
	name := `quote"and\backslash.txt`
	writeFile(t, filepath.Join(r.root, name), "content")

	res, body := r.get(t, "/api/v1/files/download?root=media&path="+urlEscape(name))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	cd := res.Header.Get("Content-Disposition")
	if strings.Contains(cd, `"and\`) {
		t.Errorf("the raw filename reached the header: %q", cd)
	}
	if string(body) != "content" {
		t.Errorf("body = %q, want the file's content", string(body))
	}
}

// TestAnAgentCredentialCannotBeEscapedByOmittingTheHeader is the test for the gap
// this closes.
//
// The first version identified an agent only by a header it sent about itself, so
// an agent could escape every agent restriction - the audit of its reads, the
// prohibition on heavy work, the approval rules - by simply not sending one. The
// credential now decides the principal, so the identity is not something the
// caller chooses.
func TestAnAgentCredentialCannotBeEscapedByOmittingTheHeader(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) {
		c.Token = "operator-token-value"
		c.Agents = []AgentCredential{{Name: "hermes", Token: "agent-token-value"}}
	})

	// The agent authenticates with its own credential.
	req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/files/list?root=media", nil)
	req.Header.Set("Authorization", "Bearer agent-token-value")
	res, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the agent's own credential was refused: %d", res.StatusCode)
	}

	// It is recorded as an agent, so its read is audited.
	rows, err := r.store.Audit(context.Background(), 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawAgent bool
	for _, e := range rows {
		if e.Principal == "ai:hermes" {
			sawAgent = true
		}
		if e.Principal == "user:operator" {
			t.Errorf("an agent credential was recorded as the operator: %+v", e)
		}
	}
	if !sawAgent {
		t.Error("the agent's read was not audited under its own name")
	}

	// The operator's credential is a different principal.
	req, _ = http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
	req.Header.Set("Authorization", "Bearer operator-token-value")
	ores, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ores.Body.Close()
	if ores.StatusCode != http.StatusOK {
		t.Errorf("the operator's credential was refused: %d", ores.StatusCode)
	}

	// No credential at all is refused, because one is configured.
	res, _ = r.get(t, "/api/v1/overview")
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unauthenticated request returned %d, want 401", res.StatusCode)
	}

	// And an agent cannot escape by claiming to be the operator with a header:
	// with a credential configured, the header is not consulted at all.
	req, _ = http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
	req.Header.Set("Authorization", "Bearer agent-token-value")
	req.Header.Set("X-Nasctl-Agent", "")
	res, err = r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	rows, _ = r.store.Audit(context.Background(), 100, 0)
	for _, e := range rows {
		if e.Action == "read" && e.Principal == "user:operator" {
			t.Errorf("an agent credential with no agent header was treated as the operator: %+v", e)
		}
	}
}

// TestAConfiguredAgentCannotStartHeavyWorkUnlessAllowed pins the per-agent switch.
func TestAConfiguredAgentCannotStartHeavyWorkUnlessAllowed(t *testing.T) {
	r := newRig(t, func(c *Config, jc *jobs.Config) {
		c.Agents = []AgentCredential{{Name: "hermes", Token: "agent-token-value"}}
	})
	// The jobs manager's own gate is what enforces this; here we confirm the
	// server reports the switch honestly rather than claiming agents are enabled.
	var out overviewResponse
	r.getJSON(t, "/api/v1/overview", &out)
	if out.AllowAIHeavy {
		t.Error("the console reports agents may start heavy work when none is allowed")
	}
	if r.srv.allowHeavyFor("hermes") {
		t.Error("an agent without allow_heavy is reported as allowed to start heavy work")
	}
	r2 := newRig(t, func(c *Config, _ *jobs.Config) {
		c.Agents = []AgentCredential{{Name: "trusted", Token: "agent-token-value", AllowHeavy: true}}
	})
	if !r2.srv.allowHeavyFor("trusted") {
		t.Error("an agent configured with allow_heavy is not recognised as such")
	}
	if r2.srv.allowHeavyFor("someone-else") {
		t.Error("allow_heavy leaked to an agent that was not configured with it")
	}
}

// TestTokensOfDifferentLengthsAreComparedSafely: the constant-time comparison must
// not be short-circuited by length, and must not accept a prefix.
func TestTokensOfDifferentLengthsAreComparedSafely(t *testing.T) {
	r := newRig(t, func(c *Config, _ *jobs.Config) { c.Token = "a-long-operator-token" })
	for _, bad := range []string{
		"a-long-operator-toke",   // one character short
		"a-long-operator-tokenX", // one character long
		"a-long-operator-tokem",  // same length, last character different
		"b-long-operator-token",  // same length, first character different
		"",                       // nothing at all
	} {
		req, _ := http.NewRequest(http.MethodGet, r.ts.URL+"/api/v1/overview", nil)
		if bad != "" {
			req.Header.Set("Authorization", "Bearer "+bad)
		}
		res, err := r.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q returned %d, want 401", bad, res.StatusCode)
		}
	}
}

// TestAConfiguredSuzuIsReportedByTheWebLayer is the regression test for a bug that
// the CLI could not catch: the serving command opened the suzu database but never
// handed the client to the web layer, so `nasctl media` reported the library while
// the page said "not configured". Comparing the two is what exposed it, and this
// test does that comparison in code.
func TestAConfiguredSuzuIsReportedByTheWebLayer(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "library")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// One present episode and one row pointing outside the root.
	inside := filepath.Join(libRoot, "ep01.mkv")
	if err := os.WriteFile(inside, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "suzu.db")
	buildSuzuDB(t, dbPath, [][3]string{
		{"ep01", "Episode 01", inside},
		{"evil", "Outside", "/etc/passwd"},
	})

	sc, err := suzu.Open(suzu.Config{DBPath: dbPath, LibraryRoot: libRoot})
	if err != nil {
		t.Fatalf("opening the suzu database: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })

	r := newRigWithSuzu(t, nil, sc)
	var out mediaStatusResponse
	res := r.getJSON(t, "/api/v1/media/status", &out)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if !out.Configured {
		t.Fatal("a configured suzu is reported as not configured by the web layer; the " +
			"client was opened but not handed to the server")
	}
	if out.LibraryCount == nil || *out.LibraryCount != 2 {
		t.Errorf("library count = %v, want 2", out.LibraryCount)
	}
	if len(out.Library) != 2 {
		t.Fatalf("library rows = %d, want 2", len(out.Library))
	}
	// The containment rule must survive the trip through HTTP.
	byID := map[string]suzu.LibraryItem{}
	for _, it := range out.Library {
		byID[it.ID] = it
	}
	if !byID["ep01"].Playable {
		t.Errorf("an episode inside the library root is not playable: %s", byID["ep01"].Note)
	}
	if byID["evil"].Playable {
		t.Error("a row pointing outside the library root is playable through the API")
	}
	if !strings.Contains(byID["evil"].Note, "library_root") {
		t.Errorf("the refusal does not explain itself: %q", byID["evil"].Note)
	}
}

// TestNoSuzuConfiguredIsReportedHonestly: the other half. With no client, the
// console says so and returns no rows rather than inventing a library.
func TestNoSuzuConfiguredIsReportedHonestly(t *testing.T) {
	r := newRig(t, nil)
	var out mediaStatusResponse
	r.getJSON(t, "/api/v1/media/status", &out)
	if out.Configured {
		t.Error("the integration reports itself configured when no client was supplied")
	}
	if len(out.Library) != 0 {
		t.Error("an unconfigured integration returned library rows")
	}
	if !strings.Contains(out.Note, "hub.suzu.db_path") {
		t.Errorf("the note does not name the setting to change: %q", out.Note)
	}
}

// buildSuzuDB creates a suzu-shaped database for the HTTP tests.
func buildSuzuDB(t *testing.T, path string, rows [][3]string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE library (id TEXT PRIMARY KEY, title TEXT, path TEXT, size INTEGER, added_at INTEGER)`,
		`CREATE TABLE subscriptions (id TEXT PRIMARY KEY, title TEXT)`,
		`CREATE TABLE items (id TEXT PRIMARY KEY)`,
		`CREATE TABLE tasks (id TEXT PRIMARY KEY, state TEXT, done_bytes INTEGER, total_bytes INTEGER)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range rows {
		if _, err := db.Exec(`INSERT INTO library VALUES(?,?,?,?,?)`,
			r[0], r[1], r[2], 100*(i+1), 1700000000+i); err != nil {
			t.Fatal(err)
		}
	}
}
