//go:build !linux

package tool

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Other platforms retain the canonical-path policy check. Rejecting symbolic
// link components again immediately before opening narrows the race window;
// platform handle-relative APIs can replace this fallback independently.
func openBeneath(root, relative string) (*os.File, error) {
	current := root
	for _, part := range splitPath(relative) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("approved path contains a symbolic link")
		}
	}
	return os.Open(filepath.Join(root, relative))
}

func splitPath(value string) []string {
	if value == "." || value == "" {
		return nil
	}
	return strings.Split(filepath.Clean(value), string(filepath.Separator))
}
