//go:build darwin

package assess

import "golang.org/x/sys/unix"

const darwinDatalessFlag = 0x40000000

func inspectStatFlags(st *unix.Stat_t) (immutable, cloudDataless bool) {
	if st == nil {
		return false, false
	}
	immutable = st.Flags&(unix.UF_IMMUTABLE|unix.UF_APPEND|unix.SF_IMMUTABLE|unix.SF_APPEND) != 0
	cloudDataless = st.Flags&darwinDatalessFlag != 0
	return immutable, cloudDataless
}
