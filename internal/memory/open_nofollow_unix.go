//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package memory

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openMemoryNoFollow(path string) (*os.File, error) {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errors.New("memory artifact must not be a symbolic link")
		}
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), path), nil
}
