package accountslocal

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/localrepair"
)

// Recover replaces only the account document, after preserving its original.
// The caller must explicitly authorize this local operation and ensure the base
// configuration still requires authentication when the state volume is absent.
// Ordinary forgotten passwords should use Save instead, preserving other users.
func Recover(ctx context.Context, state, username, password string) (localrepair.Result, error) {
	u, e := requestRecord(accounts.Request{Username: username, Role: "admin"})
	if e != nil {
		return localrepair.Result{}, e
	}
	work := &Store{slots: make(chan struct{}, 1)}
	u.Salt, u.Hash, e = work.password(ctx, password)
	if e != nil {
		return localrepair.Result{}, e
	}
	u.ID, e = randomHex()
	if e != nil {
		return localrepair.Result{}, e
	}
	u.Epoch, e = randomHex()
	if e != nil {
		return localrepair.Result{}, e
	}
	u.CreatedAt = time.Now().Unix()
	u.Algorithm = "pbkdf2-sha256"
	u.Iterations = iterations
	return localrepair.Run(ctx, state, "users.recover", func(root *os.Root, rel string) error {
		s := &Store{root: root, state: state, slots: make(chan struct{}, 1)}
		unlock, e := s.writer()
		if e != nil {
			return e
		}
		defer unlock()
		// Establish this before quarantine. Any crash before publication refuses
		// startup instead of treating the missing account document as first use.
		if e = s.enabledMarker(); e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if e = localrepair.Move(root, accounts.FileName, rel); e != nil {
			return e
		}
		if e = s.publish(ctx, document{Version: 1, Users: []record{u}}); e != nil {
			return errors.New("accounts: recovery publication failed; keep the quarantine and retry the local recovery")
		}
		_ = root.Remove(accounts.SetupFile)
		return nil
	})
}
