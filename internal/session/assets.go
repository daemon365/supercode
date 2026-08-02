package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daemon365/supercode/internal/provider"
)

const maxSessionImageBytes = 16 * 1024 * 1024

func (s *Store) externalizeSession(value Session) (Session, error) {
	value.Messages = cloneMessages(value.Messages)
	if err := s.externalizeMessages(value.ID, value.Messages); err != nil {
		return Session{}, err
	}
	return value, nil
}

func (s *Store) externalizeCheckpoint(id string, value Checkpoint) (Checkpoint, error) {
	value.Messages = cloneMessages(value.Messages)
	if err := s.externalizeMessages(id, value.Messages); err != nil {
		return Checkpoint{}, err
	}
	return value, nil
}

func (s *Store) externalizeMessages(id string, messages []provider.Message) error {
	for messageIndex := range messages {
		for imageIndex := range messages[messageIndex].Images {
			image := &messages[messageIndex].Images[imageIndex]
			if image.Data == "" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil {
				return fmt.Errorf("decode session image: %w", err)
			}
			if len(data) > maxSessionImageBytes {
				return errors.New("session image exceeds 16 MiB")
			}
			digest := sha256.Sum256(data)
			name := hex.EncodeToString(digest[:]) + imageExtension(image.MIMEType)
			directory := filepath.Join(s.assetsDirectory, id)
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return fmt.Errorf("create session asset directory: %w", err)
			}
			path := filepath.Join(directory, name)
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := atomicWrite(path, data, 0o600); err != nil {
					return fmt.Errorf("write session image: %w", err)
				}
			} else if err != nil {
				return err
			}
			image.Ref = filepath.ToSlash(filepath.Join("assets", id, name))
			image.Data = ""
		}
	}
	return nil
}

func (s *Store) hydrateMessages(messages []provider.Message) error {
	for messageIndex := range messages {
		for imageIndex := range messages[messageIndex].Images {
			image := &messages[messageIndex].Images[imageIndex]
			if image.Data != "" || image.Ref == "" {
				continue
			}
			path := filepath.Join(s.directory, filepath.FromSlash(image.Ref))
			relative, err := filepath.Rel(s.assetsDirectory, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errors.New("session image reference escapes the asset directory")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if len(data) > maxSessionImageBytes {
				return errors.New("session image exceeds 16 MiB")
			}
			image.Data = base64.StdEncoding.EncodeToString(data)
		}
	}
	return nil
}

func cloneMessages(messages []provider.Message) []provider.Message {
	result := append([]provider.Message(nil), messages...)
	for index := range result {
		result[index].ToolCalls = append([]provider.ToolCall(nil), result[index].ToolCalls...)
		result[index].Images = append([]provider.Image(nil), result[index].Images...)
	}
	return result
}

func imageExtension(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}
