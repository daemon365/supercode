//go:build linux

package tool

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openBeneath(root, relative string) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open approved read root: %w", err)
	}
	defer unix.Close(rootFD)
	fd, err := unix.Openat2(rootFD, filepath.ToSlash(relative), &unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return nil, fmt.Errorf("securely open approved path: %w", err)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(root, relative))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("securely open approved path: invalid file descriptor")
	}
	return file, nil
}
