// Package accounts is the startup-injected local account capability. Neither
// password hashes nor bootstrap credentials are part of its public views.
package accounts

import (
	"context"
	"errors"
)

const MaxUsers = 32
const FileName = "nasctl-users.json"
const SetupFile = "nasctl-setup.key"

var (
	ErrInvalid    = errors.New("accounts: invalid request or stored data")
	ErrCredential = errors.New("accounts: credentials invalid")
	ErrBusy       = errors.New("accounts: password verification busy")
	ErrConflict   = errors.New("accounts: account revision changed")
	ErrLastAdmin  = errors.New("accounts: at least one enabled administrator is required")
)

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"created_at"`
	Epoch       string `json:"-"`
}
type Snapshot struct {
	Users               []User `json:"users"`
	Revision            string `json:"revision"`
	SetupAvailable      bool   `json:"setup_available"`
	Enabled             bool   `json:"enabled"`
	MaxUsers            int    `json:"max_users"`
	SetupRequiresConfig bool   `json:"setup_requires_config,omitempty"`
}
type Request struct {
	Revision    string `json:"revision"`
	ID          string `json:"id,omitempty"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	Password    string `json:"password,omitempty"`
}
type Setup struct {
	Code        string `json:"code"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
}
type Control interface {
	Snapshot(context.Context) (Snapshot, error)
	Lookup(context.Context, string) (User, error)
	Authenticate(context.Context, string, string) (User, error)
	CreateFirst(context.Context, Setup) (User, error)
	Save(context.Context, Request) (User, error)
	Remove(context.Context, string, string) error
}
