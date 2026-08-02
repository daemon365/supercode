//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package instructions

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

var errInstructionSymlink = errors.New("project instruction file is a symbolic link")

func openInstructionNoFollow(path string) (*os.File, error) {
	descriptor, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, errInstructionSymlink
		}
		return nil, err
	}
	return os.NewFile(uintptr(descriptor), path), nil
}
