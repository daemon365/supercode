package session

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
)

func (s *Store) compressEvents(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.storeLockPath, func() error {
		return withFileLock(s.sessionLockPath(id), func() error {
			return s.compressEventsLocked(id, true)
		})
	})
}

// rotateEventsLocked keeps one compressed previous segment and starts a new
// active WAL. The snapshot must already contain every sequence in the segment.
func (s *Store) rotateEventsLocked(id string) error {
	return s.compressEventsLocked(id, false)
}

func (s *Store) compressEventsLocked(id string, archive bool) error {
	plainInfo, plainErr := os.Stat(s.eventPath(id))
	if errors.Is(plainErr, os.ErrNotExist) {
		return nil
	}
	if plainErr != nil {
		return plainErr
	}
	if plainInfo.Size() < 0 || plainInfo.Size() > maxActiveEventLogBytes {
		return errEventLogTooLarge
	}
	if plainInfo.Size() == 0 && !archive {
		return nil
	}
	temporary, err := os.CreateTemp(s.eventsDirectory, ".events-*.gz.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	writer := gzip.NewWriter(temporary)
	uncompressedBytes := int64(0)
	copyPlain := func(path string) error {
		source, openErr := os.Open(path)
		if errors.Is(openErr, os.ErrNotExist) {
			return nil
		}
		if openErr != nil {
			return openErr
		}
		defer source.Close()
		return copyEventDataLimited(writer, source, &uncompressedBytes, maxCompressedEventLogBytes)
	}
	copyCompressed := func(path string) error {
		source, openErr := os.Open(path)
		if errors.Is(openErr, os.ErrNotExist) {
			return nil
		}
		if openErr != nil {
			return openErr
		}
		reader, openErr := gzip.NewReader(source)
		if openErr != nil {
			source.Close()
			return openErr
		}
		copyErr := copyEventDataLimited(writer, reader, &uncompressedBytes, maxCompressedEventLogBytes)
		closeErr := reader.Close()
		sourceErr := source.Close()
		return errors.Join(copyErr, closeErr, sourceErr)
	}
	// Archive preserves the previous bounded segment as well. Ordinary rotation
	// intentionally replaces it so diagnostics and WAL storage remain bounded.
	if archive {
		if err := copyCompressed(s.eventGzipPath(id)); err != nil {
			writer.Close()
			temporary.Close()
			return fmt.Errorf("read compressed session events: %w", err)
		}
	}
	if err := copyPlain(s.eventPath(id)); err != nil {
		writer.Close()
		temporary.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, s.eventGzipPath(id)); err != nil {
		return err
	}
	if err := syncDirectory(s.eventsDirectory); err != nil {
		return err
	}
	if archive {
		if err := os.Remove(s.eventPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDirectory(s.eventsDirectory)
	}
	return atomicWrite(s.eventPath(id), nil, 0o600)
}

func copyEventDataLimited(destination io.Writer, source io.Reader, total *int64, maximum int64) error {
	if *total < 0 || *total > maximum {
		return errEventLogTooLarge
	}
	remaining := maximum - *total
	written, err := io.Copy(destination, io.LimitReader(source, remaining+1))
	*total += written
	if *total > maximum {
		return errEventLogTooLarge
	}
	return err
}
