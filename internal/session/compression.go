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
	source, err := os.Open(s.eventPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer source.Close()
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
	if _, err := io.Copy(writer, source); err != nil {
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
	return os.Remove(s.eventPath(id))
}

func (s *Store) restoreCompressedEventsLocked(id string) error {
	if _, err := os.Stat(s.eventPath(id)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	source, err := os.Open(s.eventGzipPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	reader, err := gzip.NewReader(source)
	if err != nil {
		source.Close()
		return err
	}
	data, err := io.ReadAll(reader)
	reader.Close()
	source.Close()
	if err != nil {
		return err
	}
	if err := atomicWrite(s.eventPath(id), data, 0o600); err != nil {
		return err
	}
	return os.Remove(s.eventGzipPath(id))
}

func (s *Store) openEventReader(id string) (io.Reader, func(), error) {
	file, err := os.Open(s.eventPath(id))
	if err == nil {
		return file, func() { _ = file.Close() }, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, func() {}, err
	}
	file, err = os.Open(s.eventGzipPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, func() {}, os.ErrNotExist
	}
	if err != nil {
		return nil, func() {}, err
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, func() {}, fmt.Errorf("open compressed session event log: %w", err)
	}
	return reader, func() { _ = reader.Close(); _ = file.Close() }, nil
}
