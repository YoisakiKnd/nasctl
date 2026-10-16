package hubweb

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"time"

	"github.com/YoisakiKnd/nasctl/internal/accounts"
	"github.com/YoisakiKnd/nasctl/internal/domain"
)

func (s *Server) accountStatus(ctx context.Context) (accounts.Snapshot, error) {
	if s.Accounts == nil {
		return accounts.Snapshot{}, nil
	}
	return s.Accounts.Snapshot(ctx)
}
func accountPrincipal(u accounts.User) domain.Principal {
	return domain.Principal{Kind: domain.PrincipalUser, Name: u.Username, UserID: u.ID, Role: u.Role}
}
func (a *browserAuth) createUser(now time.Time, u accounts.User, remember bool) (string, error) {
	return a.createSession(now, &userSession{ID: u.ID, Epoch: u.Epoch}, remember)
}
func (s *Server) browserPrincipal(r *http.Request) (domain.Principal, bool) {
	c, e := r.Cookie(sessionCookie)
	if e != nil || len(c.Value) != 43 {
		return domain.Principal{}, false
	}
	key := sha256.Sum256([]byte(c.Value))
	s.auth.mu.Lock()
	expires, ok := s.auth.sessions[key]
	user, named := s.auth.users[key]
	if !ok || !time.Now().Before(expires) {
		s.auth.drop(key)
		s.auth.mu.Unlock()
		return domain.Principal{}, false
	}
	s.auth.mu.Unlock()
	if named {
		if s.Accounts == nil {
			return domain.Principal{}, false
		}
		current, e := s.Accounts.Lookup(r.Context(), user.ID)
		if e != nil || current.Epoch != user.Epoch {
			// A changed or removed account ends the session for good,
			// including a kept one.
			if errors.Is(e, accounts.ErrCredential) || e == nil {
				s.auth.mu.Lock()
				s.auth.drop(key)
				s.auth.mu.Unlock()
			}
			return domain.Principal{}, false
		}
		return accountPrincipal(current), true
	}
	if s.Cfg.Token == "" {
		return domain.Principal{}, false
	}
	return sessionPrincipal(), true
}
