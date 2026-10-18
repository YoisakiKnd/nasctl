package hubweb

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YoisakiKnd/nasctl/internal/domain"
	"github.com/YoisakiKnd/nasctl/internal/storage"
)

type fakeStorage struct {
	calls []storage.Request
	err   error
}

func (f *fakeStorage) Available() bool { return true }

func (f *fakeStorage) Call(_ context.Context, req storage.Request, out any) error {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return f.err
	}
	switch req.Op {
	case "disks":
		*(out.(*[]storage.Disk)) = []storage.Disk{{Name: "sdb", Path: "/dev/sdb"}}
	case "status":
		*(out.(*storage.Status)) = storage.Status{Tools: map[string]bool{"smbd": true}}
	}
	return nil
}

func TestStorageIsForAdministratorsAndAudited(t *testing.T) {
	r := newRig(t, nil)
	if code, body := fileRequest(t, r, "GET", "/api/v1/storage", "", "", "", false); code != 200 || !strings.Contains(string(body), `"helper":false`) {
		t.Fatal("without helper", code, string(body))
	}
	if code, body := fileRequest(t, r, "POST", "/api/v1/storage/mount", `{"device":"/dev/sdb1"}`, "", "", true); code != 503 || !strings.Contains(string(body), "storage_helper_missing") {
		t.Fatal("missing helper", code, string(body))
	}
	fake := &fakeStorage{}
	r.srv.Storage = fake
	code, body := fileRequest(t, r, "GET", "/api/v1/storage", "", "", "", false)
	var view storageOverview
	if code != 200 || json.Unmarshal(body, &view) != nil || !view.Helper || len(view.Disks) != 1 || view.Status == nil || !view.Status.Tools["smbd"] {
		t.Fatal("overview", code, string(body))
	}
	if code, _ := fileRequest(t, r, "POST", "/api/v1/storage/mount", `{"device":"/dev/sdb1"}`, "", "", false); code != 400 {
		t.Fatal("unconfirmed mount", code)
	}
	if code, _ := fileRequest(t, r, "POST", "/api/v1/storage/rm-rf", `{}`, "", "", true); code != 404 {
		t.Fatal("unknown action", code)
	}
	if code, _ := fileRequest(t, r, "POST", "/api/v1/storage/mount", `{"device":"/dev/sdb1","shell":"x"}`, "", "", true); code != 400 {
		t.Fatal("unknown field", code)
	}
	// The URL decides the operation, never the body.
	if code, body := fileRequest(t, r, "POST", "/api/v1/storage/mount", `{"op":"format","device":"/dev/sdb1","name":"usb","persist":true}`, "", "", true); code != 200 {
		t.Fatal("mount", code, string(body))
	}
	last := fake.calls[len(fake.calls)-1]
	if last.Op != "mount" || last.Device != "/dev/sdb1" || last.Name != "usb" || !last.Persist {
		t.Fatalf("helper got %+v", last)
	}
	fake.err = &storage.HelperError{Message: "这是系统盘，不能在这里挂载或修改"}
	if code, body := fileRequest(t, r, "POST", "/api/v1/storage/share-password", `{"password":"hunter2hunter2"}`, "", "", true); code != 400 || !strings.Contains(string(body), "系统盘") {
		t.Fatal("refusal", code, string(body))
	}
	rows, err := r.store.Audit(context.Background(), 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(rows)
	if !strings.Contains(string(encoded), "storage.mount") || !strings.Contains(string(encoded), "storage.shares.password") || strings.Contains(string(encoded), "hunter2") {
		t.Fatal("audit", string(encoded))
	}

	for _, role := range []string{domain.RoleViewer, domain.RoleOperator} {
		req := httptest.NewRequest("GET", "/api/v1/storage", nil)
		req = req.WithContext(withPrincipal(req.Context(), domain.Principal{Kind: domain.PrincipalUser, UserID: "u1", Role: role}))
		w := httptest.NewRecorder()
		r.srv.handleStorage(w, req)
		if w.Code != 403 {
			t.Fatal(role, "reached storage", w.Code)
		}
	}
}
