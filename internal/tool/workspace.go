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
		path, pathErr := canonicalDenyRoot(entry)
		if pathErr != nil {
			return workspace{}, fmt.Errorf("resolve deny root %s: %w", entry, pathErr)
		}
		value.denyRoots = appendUniquePath(value.denyRoots, path)
	}
	return value, nil
}

func (w workspace) resolve(path string, allowMissing bool) (string, error) {
	return w.resolveAccess(path, allowMissing, false)
}

// openRead resolves policy first, then opens the canonical relative path from
// an allowed directory handle. On Linux the helper uses openat2 so a concurrent
// symlink swap cannot redirect the final open outside the approved root.
func (w workspace) openRead(path string) (*os.File, string, error) {
	resolved, err := w.resolve(path, false)
	if err != nil {
		return nil, "", err
	}
	root := ""
	for _, candidate := range w.allowedRoots(false) {
		if within(candidate, resolved) && len(candidate) > len(root) {
			root = candidate
		}
	}
	if root == "" || withinAny(w.denyRoots, resolved) {
		return nil, "", ErrOutsideWorkspace
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", ErrOutsideWorkspace
	}
	file, err := openBeneath(root, relative)
	if err != nil {
		return nil, "", err
	}
	return file, resolved, nil
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

func canonicalDenyRoot(path string) (string, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return "", statErr
		}
		if !info.IsDir() {
			return "", errors.New("deny root is not a directory")
		}
		return filepath.Clean(resolved), nil
	} else if !errors.Is(resolveErr, os.ErrNotExist) {
		return "", resolveErr
	}

	parent := filepath.Dir(absolute)
	for {
		resolvedParent, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			info, statErr := os.Stat(resolvedParent)
			if statErr != nil {
				return "", statErr
			}
			if !info.IsDir() {
				return "", errors.New("deny root parent is not a directory")
			}
			relative, relativeErr := filepath.Rel(parent, absolute)
			if relativeErr != nil {
				return "", relativeErr
			}
			return filepath.Clean(filepath.Join(resolvedParent, relative)), nil
		}
		if !errors.Is(resolveErr, os.ErrNotExist) {
			return "", resolveErr
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", resolveErr
		}
		parent = next
	}
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
