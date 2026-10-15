package files

import (
	"context"
	"os"
)

// BackupVault is a private, marked namespace anchored to an already-open root.
// The normal file APIs cannot name it. Only the startup-injected backup control
// calls these helpers; they do not expand the paths a browser can address.
func (m *Manager) BackupVault(ctx context.Context, rootID string, create bool) (*os.Root, error) {
	if create {
		r, unlock, err := m.lockWrite(ctx, rootID)
		if err != nil {
			return nil, err
		}
		defer unlock()
		return internalRoot(r, backupDirectory, true)
	}
	r, err := m.root(rootID)
	if err != nil {
		return nil, err
	}
	return internalRoot(r, backupDirectory, false)
}

func (m *Manager) vaultManager(vault *os.Root, writable bool) *Manager {
	roots := make(map[string]*Root, len(m.roots)+1)
	for id, r := range m.roots {
		roots[id] = r
	}
	roots["@vault"] = &Root{ID: "@vault", Path: vault.Name(), dir: vault, Writable: writable}
	return &Manager{roots: roots, limits: m.limits}
}

func (m *Manager) CopyToVault(ctx context.Context, vault *os.Root, sourceRoot, source, destination string, limit int, maxBytes int64, progress CopyProgress) (CopyResult, error) {
	return m.vaultManager(vault, true).Copy(ctx, CopyOptions{SourceRoot: sourceRoot, DestinationRoot: "@vault", From: source, To: destination, Limit: limit, MaxBytes: maxBytes, allowRoot: true}, progress)
}

// verify examines the staged copy before any destination is published.
func (m *Manager) RestoreFromVault(ctx context.Context, vault *os.Root, source, destinationRoot, destination string, limit int, maxBytes int64, verify func(context.Context, *os.Root, string) error, progress CopyProgress) (CopyResult, error) {
	return m.vaultManager(vault, false).Copy(ctx, CopyOptions{SourceRoot: "@vault", DestinationRoot: destinationRoot, From: source, To: destination, Limit: limit, MaxBytes: maxBytes, verify: verify}, progress)
}

func PublishVault(vault *os.Root, from, to string) error {
	if _, err := writablePath(from); err != nil {
		return err
	}
	if _, err := writablePath(to); err != nil {
		return err
	}
	return moveNoReplace(vault, from, vault, to)
}
func RemoveVault(ctx context.Context, vault *os.Root, rel string, limit int) error {
	if _, err := writablePath(rel); err != nil {
		return err
	}
	if _, err := vault.Lstat(rel); os.IsNotExist(err) {
		return nil
	}
	// A bundle adds its own directory and data wrapper around a 32-deep tree.
	nodes, err := planTreeDepth(ctx, vault, rel, limit, 40)
	if err != nil {
		return err
	}
	return removePlanned(ctx, vault, nodes, nil)
}
