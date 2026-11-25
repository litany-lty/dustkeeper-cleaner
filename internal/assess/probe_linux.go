//go:build linux

package assess

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func probeOpenNativeAvailable() (bool, string) {
	if _, err := os.ReadDir("/proc/self/fd"); err == nil {
		return true, "native /proc/*/fd visible-process scan"
	}
	return false, ""
}

// probeOpenPathNative checks whether any visible Linux process has an open file
// descriptor pointing to the same (device, inode) as clean.
func probeOpenPathNative(ctx context.Context, clean string) (OpenObservation, bool) {
	var target unix.Stat_t
	if err := unix.Lstat(clean, &target); err != nil {
		return OpenObservation{Paths: map[string]bool{}, Coverage: "unknown"}, false
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return OpenObservation{Paths: map[string]bool{}, Coverage: "unknown"}, false
	}
	out := OpenObservation{Paths: map[string]bool{}, Coverage: "visible_processes_only"}
	for _, proc := range procs {
		if ctx.Err() != nil {
			out.Coverage = "unknown"
			if len(out.Paths) > 0 {
				out.Coverage = "partial_visible_processes"
			}
			return out, true
		}
		if !proc.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(proc.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join("/proc", proc.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// Unprivileged processes cannot read other users' /proc/<pid>/fd;
			// matching lsof's visible_processes_only semantics.
			continue
		}
		for _, fd := range fds {
			var st unix.Stat_t
			if err := unix.Stat(filepath.Join(fdDir, fd.Name()), &st); err != nil {
				continue
			}
			if st.Dev == target.Dev && st.Ino == target.Ino {
				out.Paths[clean] = true
				out.VisibleMatches = 1
				return out, true
			}
		}
	}
	return out, true
}
