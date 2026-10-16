package files

import (
	"errors"
	"os"
	"path"
	"syscall"
)

// Resolve returns where a root-relative path really lives after in-root links
// are followed. A path that does not exist yet resolves through its parent, so
// a destination can be judged before anything is written there.
func (m *Manager) Resolve(rootID, rel string) (string, error) {
	r, err := m.root(rootID)
	if err != nil {
		return "", err
	}
	clean, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	f, real, err := safeOpenResolved(r.dir, clean, os.O_RDONLY|syscall.O_NONBLOCK)
	if err == nil {
		_ = f.Close()
		return real, nil
	}
	if !errors.Is(err, ErrNotExist) || clean == "." {
		return "", err
	}
	parent, err := m.Resolve(rootID, path.Dir(clean))
	if err != nil {
		return "", err
	}
	return path.Join(parent, path.Base(clean)), nil
}

// TrashEntry reads one trash record, so a caller can check where it came from
// before restoring or purging it.
func (m *Manager) TrashEntry(rootID, id string) (TrashItem, error) {
	if !validFileID(id) {
		return TrashItem{}, ErrBadPath
	}
	r, err := m.root(rootID)
	if err != nil {
		return TrashItem{}, err
	}
	trash, err := internalRoot(r, trashDirectory, false)
	if err != nil {
		return TrashItem{}, err
	}
	defer trash.Close()
	item, present, err := itemInTrash(trash, id)
	if err != nil {
		return TrashItem{}, mapWriteErr(err)
	}
	if !present {
		return TrashItem{}, ErrNotExist
	}
	return item, nil
}
