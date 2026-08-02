// Package instructions discovers directory-scoped project instruction files.
package instructions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultMaxBytes = 64 * 1024

type Options struct {
	Root          string
	CWD           string
	FallbackNames []string
	MaxBytes      int
}

type Document struct {
	Path    string
	Content string
}

type Set struct {
	Root      string
	Documents []Document
}

func (s Set) Sources() []string {
	result := make([]string, 0, len(s.Documents))
	for _, document := range s.Documents {
		result = append(result, document.Path)
	}
	return result
}

func (s Set) Text() string {
	var result []string
	for _, document := range s.Documents {
		result = append(result, fmt.Sprintf("Project instructions from %s:\n%s", document.Path, document.Content))
	}
	return strings.Join(result, "\n\n")
}

// Discover walks from Root to CWD. In each directory, AGENTS.override.md wins
// over AGENTS.md and configured fallbacks. Files are never followed through a
// symlink, and the combined byte budget is enforced in discovery order.
func Discover(options Options) (Set, error) {
	root, cwd, err := normalizeBounds(options.Root, options.CWD)
	if err != nil {
		return Set{}, err
	}
	maximum := options.MaxBytes
	if maximum <= 0 {
		maximum = DefaultMaxBytes
	}
	names := append([]string{"AGENTS.override.md", "AGENTS.md"}, options.FallbackNames...)
	directories := pathFromRoot(root, cwd)
	result := Set{Root: root}
	used := 0
	for _, directory := range directories {
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name == "" || filepath.Base(name) != name {
				continue
			}
			path := filepath.Join(directory, name)
			info, statErr := os.Lstat(path)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					continue
				}
				return Set{}, fmt.Errorf("inspect project instructions %s: %w", path, statErr)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				continue
			}
			if info.Size() > int64(maximum-used) {
				return Set{}, fmt.Errorf("project instructions exceed the %d-byte limit at %s", maximum, path)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return Set{}, fmt.Errorf("read project instructions %s: %w", path, readErr)
			}
			used += len(content)
			result.Documents = append(result.Documents, Document{Path: path, Content: string(content)})
			break
		}
	}
	return result, nil
}

func FindProjectRoot(path string) string {
	current, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	for {
		if info, statErr := os.Stat(filepath.Join(current, ".git")); statErr == nil && info.IsDir() {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		current = parent
	}
}

func normalizeBounds(root, cwd string) (string, string, error) {
	if strings.TrimSpace(cwd) == "" {
		cwd = root
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(root, cwd)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("instruction cwd %s is outside root %s", cwd, root)
	}
	return filepath.Clean(root), filepath.Clean(cwd), nil
}

func pathFromRoot(root, cwd string) []string {
	var reverse []string
	for current := cwd; ; current = filepath.Dir(current) {
		reverse = append(reverse, current)
		if current == root {
			break
		}
	}
	result := make([]string, len(reverse))
	for index := range reverse {
		result[len(reverse)-1-index] = reverse[index]
	}
	return result
}
