//go:build !linux && !darwin

package files

func renameExclusive(int, string, int, string) error { return ErrUnsupported }
