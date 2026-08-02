//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package policy

import "os"

func syncPolicyDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
