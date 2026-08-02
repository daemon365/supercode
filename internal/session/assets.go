package session

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/daemon365/supercode/internal/attachment"
	"github.com/daemon365/supercode/internal/provider"
)

const (
	maxSessionImageBytes         = attachment.MaxImageBytes
	maxSessionHydratedImages     = 32
	maxSessionHydratedImageBytes = int64(128 * 1024 * 1024)
)

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
			if len(image.Data) > base64.StdEncoding.EncodedLen(maxSessionImageBytes) {
				return sessionImageTooLargeError()
			}
			data, err := base64.StdEncoding.DecodeString(image.Data)
			if err != nil {
				return fmt.Errorf("decode session image: %w", err)
			}
			if len(data) > maxSessionImageBytes {
				return sessionImageTooLargeError()
			}
			digest := sha256.Sum256(data)
			name := hex.EncodeToString(digest[:]) + imageExtension(image.MIMEType)
			directory := filepath.Join(s.assetsDirectory, id)
			info, statErr := os.Lstat(directory)
			created := errors.Is(statErr, os.ErrNotExist)
			if statErr != nil && !created {
				return fmt.Errorf("inspect session asset directory: %w", statErr)
			}
			if !created && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
				return errors.New("session asset path is not a regular directory")
			}
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return fmt.Errorf("create session asset directory: %w", err)
			}
			if err := os.Chmod(directory, 0o700); err != nil {
				return fmt.Errorf("secure session asset directory: %w", err)
			}
			if created {
				if err := syncDirectory(s.assetsDirectory); err != nil {
					return fmt.Errorf("sync session asset directory: %w", err)
				}
			}
			path := filepath.Join(directory, name)
			if info, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
				if err := atomicWrite(path, data, 0o600); err != nil {
					return fmt.Errorf("write session image: %w", err)
				}
			} else if err != nil {
				return err
			} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return errors.New("session asset is not a regular file")
			}
			image.Ref = filepath.ToSlash(filepath.Join("assets", id, name))
			image.Data = ""
		}
	}
	return nil
}

func (s *Store) hydrateMessages(messages []provider.Message) error {
	imageCount := 0
	var hydratedBytes int64
	for messageIndex := range messages {
		for imageIndex := range messages[messageIndex].Images {
			image := &messages[messageIndex].Images[imageIndex]
			if image.Data != "" {
				if err := reserveSessionImage(&imageCount, &hydratedBytes, decodedBase64Length(image.Data)); err != nil {
					return err
				}
				continue
			}
			if image.Ref == "" {
				continue
			}
			path := filepath.Join(s.directory, filepath.FromSlash(image.Ref))
			relative, err := filepath.Rel(s.assetsDirectory, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return errors.New("session image reference escapes the asset directory")
			}
			before, err := inspectAssetPath(s.assetsDirectory, path)
			if err != nil {
				return err
			}
			if err := reserveSessionImage(&imageCount, &hydratedBytes, before.Size()); err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			after, statErr := file.Stat()
			if statErr != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > maxSessionImageBytes {
				_ = file.Close()
				if statErr != nil {
					return statErr
				}
				if after.Size() > maxSessionImageBytes {
					return sessionImageTooLargeError()
				}
				return errors.New("session image changed while opening")
			}
			data, readErr := io.ReadAll(io.LimitReader(file, maxSessionImageBytes+1))
			closeErr := file.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
			if len(data) > maxSessionImageBytes {
				return sessionImageTooLargeError()
			}
			name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if len(name) == sha256.Size*2 {
				digest := sha256.Sum256(data)
				if !strings.EqualFold(name, hex.EncodeToString(digest[:])) {
					return errors.New("session image content does not match its reference")
				}
			}
			image.Data = base64.StdEncoding.EncodeToString(data)
		}
	}
	return nil
}

func reserveSessionImage(count *int, total *int64, size int64) error {
	if size < 0 || size > maxSessionImageBytes {
		return sessionImageTooLargeError()
	}
	if *count >= maxSessionHydratedImages || size > maxSessionHydratedImageBytes-*total {
		return fmt.Errorf("session images exceed the aggregate limit of %d images or %d MiB", maxSessionHydratedImages, maxSessionHydratedImageBytes/(1024*1024))
	}
	*count++
	*total += size
	return nil
}

func decodedBase64Length(value string) int64 {
	decoded := int64(base64.StdEncoding.DecodedLen(len(value)))
	if strings.HasSuffix(value, "=") {
		decoded--
	}
	if strings.HasSuffix(value, "==") {
		decoded--
	}
	return max(0, decoded)
}

func inspectAssetPath(root, path string) (os.FileInfo, error) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, errors.New("session image reference escapes the asset directory")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("session asset root is not a regular directory")
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	var info os.FileInfo
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, errors.New("invalid session image reference")
		}
		current = filepath.Join(current, part)
		info, err = os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("session image reference contains a symbolic link")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return nil, errors.New("session image reference contains a non-directory component")
		}
	}
	if info == nil || !info.Mode().IsRegular() {
		return nil, errors.New("session image is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxSessionImageBytes {
		return nil, sessionImageTooLargeError()
	}
	return info, nil
}

func sessionImageTooLargeError() error {
	return fmt.Errorf("session image exceeds %d MiB", maxSessionImageBytes/(1024*1024))
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
