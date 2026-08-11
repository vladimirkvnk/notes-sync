//go:build darwin || linux

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock is an advisory process lock released automatically when its file is
// closed, including after a crash.
type Lock struct {
	file *os.File
}

// Acquire obtains a non-blocking exclusive lock.
func Acquire(path string) (*Lock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("repository is already managed by another notes-sync process")
		}
		return nil, fmt.Errorf("lock repository: %w", err)
	}
	return &Lock{file: file}, nil
}

// Close releases the lock and its descriptor.
func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return fmt.Errorf("unlock repository: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file: %w", closeErr)
	}
	return nil
}
