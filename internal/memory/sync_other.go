//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package memory

func syncMemoryDirectory(string) error { return nil }
