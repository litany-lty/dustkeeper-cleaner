//go:build darwin

package inventory

import "golang.org/x/sys/unix"

const darwinSFDataless = 0x40000000

func isCloudDataless(st *unix.Stat_t) bool {
	return st.Flags&darwinSFDataless != 0
}
