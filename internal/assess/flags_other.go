//go:build !darwin

package assess

import "golang.org/x/sys/unix"

func inspectStatFlags(*unix.Stat_t) (immutable, cloudDataless bool) {
	return false, false
}
