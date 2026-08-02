//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package policy

func syncPolicyDirectory(string) error { return nil }
