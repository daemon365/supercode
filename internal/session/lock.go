package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func withFileLock(path string, operation func() error) error {
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open session lock: %w", err)
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure session lock: %w", err)
	}
	if created {
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync session lock: %w", err)
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return fmt.Errorf("sync session lock directory: %w", err)
		}
	}
	if err := lockFile(file); err != nil {
		return fmt.Errorf("lock session storage: %w", err)
	}
	defer unlockFile(file)
	return operation()
}
