package photovault

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/album"
)

func vaultRig(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	state := t.TempDir()
	s, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	password := "disposable-vault-password"
	if e = s.Create(password); e != nil {
		t.Fatal(e)
	}
	token, e := s.Unlock(password, "owner")
	if e != nil {
		t.Fatal(e)
	}
	return s, token, password, state
}
func putPhoto(t *testing.T, s *Store, token string) (string, []byte) {
	t.Helper()
	body := bytes.Repeat([]byte("PRIVATE-PHOTO-CONTENT"), 12345)
	record := Record{Details: album.Details{Item: album.Item{ID: 7, Kind: "image", Name: "PRIVATE-NAME.jpg", Bytes: int64(len(body)), MIME: "image/jpeg"}}, SourceRoot: "PRIVATE-ROOT", SourcePath: "PRIVATE-PATH/PRIVATE-NAME.jpg", MtimeNS: 33, ThumbMIME: "image/jpeg"}
	id, hash, e := s.Put(context.Background(), token, "owner", record, bytes.NewReader(body), []byte("PRIVATE-THUMB"))
	sum := sha256.Sum256(body)
	if e != nil || hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("put: %s %v", hash, e)
	}
	return id, body
}
func TestEncryptedStorageRangesLockReopenAndPasswordRotation(t *testing.T) {
	s, token, password, state := vaultRig(t)
	id, body := putPhoto(t, s, token)
	if !s.Protected(7) {
		t.Fatal("pending original is not guarded")
	}
	entries, e := os.ReadDir(Path(state))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		b, e := os.ReadFile(filepath.Join(Path(state), entry.Name()))
		if e != nil {
			t.Fatal(e)
		}
		for _, secret := range []string{password, "PRIVATE-NAME", "PRIVATE-PATH", "PRIVATE-ROOT", "PRIVATE-PHOTO-CONTENT", "PRIVATE-THUMB"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Fatalf("plaintext %q in %s", secret, entry.Name())
			}
		}
	}
	if _, _, e = s.List(context.Background(), token, "another-owner", 1); !errors.Is(e, ErrLocked) {
		t.Fatalf("owner bypass: %v", e)
	}
	reader, item, _, e := s.OpenMedia(context.Background(), token, "owner", id, "raw")
	if e != nil || item.ID != id {
		t.Fatal(e)
	}
	if _, e = reader.Seek(chunkSize-9, io.SeekStart); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 40)
	if _, e = io.ReadFull(reader, b); e != nil || !bytes.Equal(b, body[chunkSize-9:chunkSize+31]) {
		t.Fatalf("cross-chunk range %v", e)
	}
	s.Lock(token, "owner")
	if _, e = reader.Read(b); !errors.Is(e, ErrLocked) {
		t.Fatalf("open stream survives lock: %v", e)
	}
	reader.Close()
	token, e = s.Unlock(password, "owner")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Complete(token, "owner", id); e != nil || s.Protected(7) {
		t.Fatal("completion", e)
	}
	if e = s.ChangePassword(token, "owner", password, "new-disposable-password"); e != nil {
		t.Fatal(e)
	}
	if s.Status(token, "owner").Unlocked {
		t.Fatal("rotation left session unlocked")
	}
	if _, e = s.Unlock(password, "owner"); !errors.Is(e, ErrLocked) {
		t.Fatal("old password works", e)
	}
	s.Close()
	s, e = Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if !s.Status("", "owner").Configured || s.Status(token, "owner").Unlocked {
		t.Fatal("restart state")
	}
	token, e = s.Unlock("new-disposable-password", "owner")
	if e != nil {
		t.Fatal(e)
	}
	items, total, e := s.List(context.Background(), token, "owner", 1)
	if e != nil || total != 1 || len(items) != 1 || items[0].Pending {
		t.Fatal("reopen listing", items, total, e)
	}
	serialized, _ := json.Marshal(items[0])
	var view map[string]any
	json.Unmarshal(serialized, &view)
	if view["id"] != id || view["name"] != "PRIVATE-NAME.jpg" {
		t.Fatal("public JSON ID collision", string(serialized))
	}
	r, _, _, e := s.OpenMedia(context.Background(), token, "owner", id, "download")
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(r)
	r.Close()
	if e != nil || !bytes.Equal(got, body) {
		t.Fatal("round trip", e)
	}
	s.mu.Lock()
	s.sessions[sha256.Sum256([]byte(token))].expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if _, _, e = s.List(context.Background(), token, "owner", 1); !errors.Is(e, ErrLocked) {
		t.Fatal("expired session", e)
	}
}
func TestCiphertextTamperingSwapTruncationAndCancelledPut(t *testing.T) {
	s, token, _, state := vaultRig(t)
	defer s.Close()
	id, _ := putPhoto(t, s, token)
	f, e := os.OpenFile(filepath.Join(Path(state), id+".original"), os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	var b [1]byte
	f.ReadAt(b[:], 30)
	b[0] ^= 1
	f.WriteAt(b[:], 30)
	f.Close()
	r, _, _, e := s.OpenMedia(context.Background(), token, "owner", id, "raw")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.ReadAll(r); !errors.Is(e, ErrInvalid) {
		t.Fatal("tampering accepted", e)
	}
	r.Close()
	// A valid ciphertext from another part must also fail authentication.
	thumb, e := os.ReadFile(filepath.Join(Path(state), id+".thumb"))
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(Path(state), id+".original"), thumb, 0600)
	swapped, _, _, e := s.OpenMedia(context.Background(), token, "owner", id, "raw")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = io.ReadAll(swapped); !errors.Is(e, ErrInvalid) {
		t.Fatal("part swap accepted", e)
	}
	swapped.Close()
	os.WriteFile(filepath.Join(Path(state), id+".thumb"), []byte("truncated"), 0600)
	if _, _, _, e = s.OpenMedia(context.Background(), token, "owner", id, "thumb"); !errors.Is(e, ErrInvalid) {
		t.Fatal("truncation accepted", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, e = s.Put(ctx, token, "owner", Record{Details: album.Details{Item: album.Item{ID: 8, Kind: "image", Bytes: 1}}}, bytes.NewReader([]byte{1}), []byte{2})
	if !errors.Is(e, context.Canceled) {
		t.Fatal("cancel", e)
	}
	items, total, e := s.List(context.Background(), token, "owner", 1)
	if e != nil || total != 1 || len(items) != 1 {
		t.Fatal("cancel published partial data", e)
	}
	if _, e = s.Unlock("wrong", "owner"); !errors.Is(e, ErrLocked) {
		t.Fatal("bad password", e)
	}
}
func TestVaultRejectsNamespaceSymlinks(t *testing.T) {
	state := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(state, Directory)); e != nil {
		t.Fatal(e)
	}
	if s, e := Open(state); e == nil {
		s.Close()
		t.Fatal("adopted linked vault")
	}
}

func TestPublishedIndexSyncFailureKeepsCiphertextAndRevokesRotatedSessions(t *testing.T) {
	s, token, password, state := vaultRig(t)
	failure := errors.New("simulated directory sync failure")
	calls := 0
	// First sync publishes payloads; second is after the index rename.
	s.syncDirectory = func() error {
		calls++
		if calls == 2 {
			return failure
		}
		return nil
	}
	body := []byte("original survives publication sync failure")
	record := Record{Details: album.Details{Item: album.Item{ID: 9, Name: "private.jpg", Kind: "image", MIME: "image/jpeg", Bytes: int64(len(body))}}, ThumbMIME: "image/jpeg"}
	id, _, e := s.Put(context.Background(), token, "owner", record, bytes.NewReader(body), []byte("thumb"))
	if !errors.Is(e, failure) || id == "" || !s.Protected(9) {
		t.Fatal("published partial state not retained", id, e)
	}
	s.syncDirectory = nil
	if e = s.Verify(context.Background(), token, "owner", id); e != nil {
		t.Fatal("ciphertext lost after index publication", e)
	}
	s.syncDirectory = func() error { return failure }
	if e = s.ChangePassword(token, "owner", password, "another generated password"); !errors.Is(e, failure) {
		t.Fatal(e)
	}
	if s.Status(token, "owner").Unlocked {
		t.Fatal("published password rotation retained old sessions")
	}
	s.syncDirectory = nil
	s.Close()
	reopened, e := Open(state)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	next, e := reopened.Unlock("another generated password", "owner")
	if e != nil {
		t.Fatal("published password not usable after reopen", e)
	}
	if e = reopened.Verify(context.Background(), next, "owner", id); e != nil {
		t.Fatal("reopened ciphertext invalid", e)
	}
}
