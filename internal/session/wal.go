package session

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type sessionHead struct {
	Version      int    `json:"version"`
	Sequence     uint64 `json:"sequence"`
	Revision     uint64 `json:"revision"`
	MessageCount int    `json:"message_count"`
}

const (
	maxEventRecordBytes        = int64(64 * 1024 * 1024)
	maxActiveEventLogBytes     = int64(128 * 1024 * 1024)
	maxCompressedEventLogBytes = int64(256 * 1024 * 1024)
)

var (
	errEventRecordTooLarge = errors.New("session event record exceeds 64 MiB")
	errEventLogTooLarge    = errors.New("session event log exceeds its safety limit")
)

func (s *Store) readHeadLocked(id string) (sessionHead, bool, error) {
	data, err := os.ReadFile(s.headPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return sessionHead{}, false, nil
	}
	if err != nil {
		return sessionHead{}, false, fmt.Errorf("read session head: %w", err)
	}
	var head sessionHead
	if err := json.Unmarshal(data, &head); err != nil || head.Version != 1 {
		// The head is only a small reconciliation cache. Snapshot and WAL records
		// remain authoritative and the next successful operation rewrites it.
		return sessionHead{}, false, nil
	}
	return head, true, nil
}

func (s *Store) writeHeadLocked(id string, state commitState) error {
	data, err := json.Marshal(sessionHead{
		Version: 1, Sequence: state.sequence, Revision: state.revision, MessageCount: len(state.messages),
	})
	if err != nil {
		return fmt.Errorf("encode session head: %w", err)
	}
	if err := atomicWrite(s.headPath(id), data, 0o600); err != nil {
		return fmt.Errorf("write session head: %w", err)
	}
	return nil
}

func (s *Store) reconcileHeadLocked(id string, state commitState) error {
	head, exists, err := s.readHeadLocked(id)
	if err != nil {
		return err
	}
	if exists && head.Sequence == state.sequence && head.Revision == state.revision && head.MessageCount == len(state.messages) {
		return nil
	}
	return s.writeHeadLocked(id, state)
}

// repairEventTailLocked makes the active JSONL appendable after a short write.
// A complete JSON record that only lacks its newline is preserved; an invalid
// unterminated suffix is truncated. Complete corrupt lines are rejected by the
// reader and are never silently discarded.
func (s *Store) repairEventTailLocked(id string) error {
	path := s.eventPath(id)
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open session event log for repair: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return err
	}
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		if _, _, err := readLastRecord(file, info.Size()); err != nil {
			return fmt.Errorf("inspect session event log final record: %w", err)
		}
		return nil
	}
	start, tail, err := readLastUnterminated(file, info.Size())
	if err != nil {
		return err
	}
	var event Event
	if json.Unmarshal(bytes.TrimSpace(tail), &event) == nil {
		if _, err := file.WriteAt([]byte{'\n'}, info.Size()); err != nil {
			return fmt.Errorf("finish session event record: %w", err)
		}
	} else if err := file.Truncate(start); err != nil {
		return fmt.Errorf("truncate incomplete session event record: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync repaired session event log: %w", err)
	}
	return nil
}

func readLastUnterminated(file *os.File, size int64) (int64, []byte, error) {
	return readLastUnterminatedLimit(file, size, maxEventRecordBytes)
}

func readLastUnterminatedLimit(file *os.File, size, maximum int64) (int64, []byte, error) {
	const blockSize = int64(64 * 1024)
	if size < 0 || maximum < 0 {
		return 0, nil, errEventRecordTooLarge
	}
	lowerBound := max(int64(0), size-maximum-1)
	cursor := size
	for cursor > lowerBound {
		start := max(lowerBound, cursor-blockSize)
		amount := cursor - start
		block := make([]byte, amount)
		if _, err := file.ReadAt(block, start); err != nil {
			return 0, nil, err
		}
		if index := bytes.LastIndexByte(block, '\n'); index >= 0 {
			recordStart := start + int64(index) + 1
			length := size - recordStart
			if length > maximum {
				return 0, nil, errEventRecordTooLarge
			}
			tail := make([]byte, length)
			if length > 0 {
				if _, err := file.ReadAt(tail, recordStart); err != nil {
					return 0, nil, err
				}
			}
			return recordStart, tail, nil
		}
		cursor = start
	}
	if lowerBound > 0 || size > maximum {
		return 0, nil, errEventRecordTooLarge
	}
	tail := make([]byte, size)
	if size > 0 {
		if _, err := file.ReadAt(tail, 0); err != nil {
			return 0, nil, err
		}
	}
	return 0, tail, nil
}

func (s *Store) lastPlainEventLocked(id string) (Event, bool, error) {
	if err := s.repairEventTailLocked(id); err != nil {
		return Event{}, false, err
	}
	file, err := os.Open(s.eventPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return Event{}, false, err
	}
	_, tail, err := readLastRecord(file, info.Size())
	if err != nil {
		return Event{}, false, err
	}
	var event Event
	if err := json.Unmarshal(bytes.TrimSpace(tail), &event); err != nil {
		return Event{}, false, fmt.Errorf("corrupt session event log final record: %w", err)
	}
	if strings.TrimSpace(event.Type) == "" || event.At.IsZero() {
		return Event{}, false, corruption("session event log final record is missing type or timestamp")
	}
	return event, true, nil
}

func readLastRecord(file *os.File, size int64) (int64, []byte, error) {
	if size > 0 {
		last := []byte{0}
		if _, err := file.ReadAt(last, size-1); err != nil {
			return 0, nil, err
		}
		if last[0] == '\n' {
			size--
		}
	}
	return readLastUnterminated(file, size)
}

func (s *Store) appendEventRecordLocked(id string, event Event) (int64, error) {
	if err := s.repairEventTailLocked(id); err != nil {
		return 0, err
	}
	data, err := json.Marshal(event)
	if err != nil {
		return 0, fmt.Errorf("encode session event: %w", err)
	}
	if int64(len(data)) > maxEventRecordBytes {
		return 0, errEventRecordTooLarge
	}
	path := s.eventPath(id)
	info, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return 0, fmt.Errorf("inspect session event log: %w", statErr)
	}
	currentSize := int64(0)
	if !created {
		currentSize = info.Size()
	}
	recordSize := int64(len(data)) + 1
	if currentSize < 0 || currentSize > maxActiveEventLogBytes-recordSize {
		return 0, errEventLogTooLarge
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open session event log: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return 0, err
	}
	record := append(data, '\n')
	written, err := file.Write(record)
	if err == nil && written != len(record) {
		err = io.ErrShortWrite
	}
	if err != nil {
		file.Close()
		return 0, fmt.Errorf("append session event: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, fmt.Errorf("sync session event: %w", err)
	}
	if err := file.Close(); err != nil {
		return 0, err
	}
	if created {
		if err := syncDirectory(s.eventsDirectory); err != nil {
			return 0, fmt.Errorf("sync session events directory: %w", err)
		}
	}
	return int64(len(record)), nil
}

func (s *Store) readEventsLocked(id string) ([]Event, error) {
	if err := s.repairEventTailLocked(id); err != nil {
		return nil, err
	}
	var events []Event
	seen := make(map[uint64][]byte)
	read := func(reader io.Reader, source string, maximum int64) error {
		buffered := bufio.NewReaderSize(reader, 64*1024)
		line := 0
		total := int64(0)
		for {
			record, readErr := readEventRecordLimited(buffered, &total, maximum, maxEventRecordBytes)
			if errors.Is(readErr, errEventRecordTooLarge) || errors.Is(readErr, errEventLogTooLarge) {
				return fmt.Errorf("read session event log %s: %w", source, readErr)
			}
			if len(record) > 0 {
				line++
				trimmed := bytes.TrimSpace(record)
				if len(trimmed) > 0 {
					var event Event
					if err := json.Unmarshal(trimmed, &event); err != nil {
						return fmt.Errorf("corrupt session event log %s line %d: %w", source, line, err)
					}
					if strings.TrimSpace(event.Type) == "" || event.At.IsZero() {
						return corruption("event log %s line %d is missing type or timestamp", source, line)
					}
					if event.Sequence > 0 {
						if previous, duplicate := seen[event.Sequence]; duplicate {
							if !bytes.Equal(previous, trimmed) {
								return corruption("event sequence %d has conflicting records", event.Sequence)
							}
							if readErr == io.EOF {
								return nil
							}
							continue
						}
						seen[event.Sequence] = append([]byte(nil), trimmed...)
					}
					events = append(events, event)
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return fmt.Errorf("read session event log %s: %w", source, readErr)
			}
		}
	}

	compressed, err := os.Open(s.eventGzipPath(id))
	if err == nil {
		reader, gzipErr := gzip.NewReader(compressed)
		if gzipErr != nil {
			compressed.Close()
			return nil, fmt.Errorf("open compressed session event log: %w", gzipErr)
		}
		if err := read(reader, filepath.Base(s.eventGzipPath(id)), maxCompressedEventLogBytes); err != nil {
			reader.Close()
			compressed.Close()
			return nil, err
		}
		if err := reader.Close(); err != nil {
			compressed.Close()
			return nil, err
		}
		if err := compressed.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	plain, err := os.Open(s.eventPath(id))
	if err == nil {
		defer plain.Close()
		if err := read(plain, filepath.Base(s.eventPath(id)), maxActiveEventLogBytes); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return events, nil
}

func readEventRecordLimited(reader *bufio.Reader, total *int64, maximumTotal, maximumRecord int64) ([]byte, error) {
	record := make([]byte, 0, min(int64(64*1024), maximumRecord))
	for {
		fragment, err := reader.ReadSlice('\n')
		*total += int64(len(fragment))
		if *total > maximumTotal {
			return nil, errEventLogTooLarge
		}
		projected := int64(len(record)) + int64(len(fragment))
		allowed := maximumRecord
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			allowed++
		}
		if projected > allowed {
			return nil, errEventRecordTooLarge
		}
		record = append(record, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return record, err
	}
}

func eventLogSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func corruption(message string, arguments ...any) error {
	return fmt.Errorf("session data is corrupt: "+strings.TrimSpace(message), arguments...)
}
