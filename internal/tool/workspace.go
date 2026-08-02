package tool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daemon365/supercode/internal/permission"
)

var ErrOutsideWorkspace = errors.New("path is outside the workspace")

type workspace struct {
	root        string
	readRoots   []string
	writeRoots  []string
	denyRoots   []string
	permissions *permission.Manager
}

func newWorkspace(root string) (workspace, error) {
	return newWorkspaceWithOptions(root, SandboxOptions{})
}

func newWorkspaceWithOptions(root string, options SandboxOptions) (workspace, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return workspace{}, fmt.Errorf("resolve workspace: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return workspace{}, fmt.Errorf("resolve workspace links: %w", err)
	}
	resolved = filepath.Clean(resolved)
	value := workspace{root: resolved, readRoots: []string{resolved}, writeRoots: []string{resolved}, permissions: options.Permissions}
	for _, entry := range options.ReadRoots {
		path, pathErr := canonicalDirectory(entry)
		if pathErr != nil {
			return workspace{}, fmt.Errorf("resolve read root %s: %w", entry, pathErr)
		}
		value.readRoots = appendUniquePath(value.readRoots, path)
	}
	for _, entry := range options.WriteRoots {
		path, pathErr := canonicalDirectory(entry)
		if pathErr != nil {
			return workspace{}, fmt.Errorf("resolve write root %s: %w", entry, pathErr)
		}
		value.writeRoots = appendUniquePath(value.writeRoots, path)
		value.readRoots = appendUniquePath(value.readRoots, path)
	}
	for _, entry := range options.DenyRoots {
		path, pathErr := filepath.Abs(entry)
		if pathErr != nil {
			return workspace{}, fmt.Errorf("resolve deny root %s: %w", entry, pathErr)
		}
		value.denyRoots = appendUniquePath(value.denyRoots, filepath.Clean(path))
	}
	return value, nil
}

func (w workspace) resolve(path string, allowMissing bool) (string, error) {
	return w.resolveAccess(path, allowMissing, false)
}

func (w workspace) resolveWrite(path string, allowMissing bool) (string, error) {
	return w.resolveAccess(path, allowMissing, true)
}

func (w workspace) resolveAccess(path string, allowMissing, write bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return w.root, nil
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(w.root, candidate)
	}
	candidate = filepath.Clean(candidate)
	allowedRoots := w.allowedRoots(write)
	if !withinAny(allowedRoots, candidate) || withinAny(w.denyRoots, candidate) {
		return "", ErrOutsideWorkspace
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err == nil {
		if !withinAny(allowedRoots, resolved) || withinAny(w.denyRoots, resolved) {
			return "", ErrOutsideWorkspace
		}
		return resolved, nil
	}
	if !allowMissing || !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	parent := filepath.Dir(candidate)
	for withinAny(allowedRoots, parent) {
		resolvedParent, parentErr := filepath.EvalSymlinks(parent)
		if parentErr == nil {
			if !withinAny(allowedRoots, resolvedParent) || withinAny(w.denyRoots, resolvedParent) {
				return "", ErrOutsideWorkspace
			}
			break
		}
		if !errors.Is(parentErr, os.ErrNotExist) {
			return "", parentErr
		}
		next := filepath.Dir(parent)
		if next == parent || !withinAny(allowedRoots, next) {
			return "", ErrOutsideWorkspace
		}
		parent = next
	}
	return candidate, nil
}

func (w workspace) allowedRoots(write bool) []string {
	roots := append([]string(nil), w.readRoots...)
	if write {
		roots = append([]string(nil), w.writeRoots...)
	}
	if w.permissions == nil {
		return roots
	}
	dynamic := w.permissions.ReadRoots()
	if write {
		dynamic = w.permissions.WriteRoots()
	}
	for _, root := range dynamic {
		roots = appendUniquePath(roots, root)
	}
	return roots
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("root is not a directory")
	}
	return filepath.Clean(resolved), nil
}

func appendUniquePath(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}

func withinAny(roots []string, candidate string) bool {
	for _, root := range roots {
		if within(root, candidate) {
			return true
		}
	}
	return false
}

func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (w workspace) display(path string) string {
	relative, err := filepath.Rel(w.root, path)
	if err != nil || relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}
