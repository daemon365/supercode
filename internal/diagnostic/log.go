package diagnostic

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	path string
	mu   sync.Mutex
}

func NewLogger(path string) (*Logger, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("debug log path is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return &Logger{path: path}, nil
}

func (l *Logger) Path() string { return l.path }

func (l *Logger) Log(event string, fields map[string]any) {
	if l == nil {
		return
	}
	value := make(map[string]any, len(fields)+2)
	value["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	value["event"] = event
	for key, item := range fields {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") {
			value[key] = "[redacted]"
			continue
		}
		value[key] = item
	}
	content, err := json.Marshal(value)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = file.Write(append(content, '\n'))
	_ = file.Close()
}
