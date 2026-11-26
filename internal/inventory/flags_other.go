//go:build !darwin

package inventory

import "golang.org/x/sys/unix"

func isCloudDataless(_ *unix.Stat_t) bool {
	return false
}
