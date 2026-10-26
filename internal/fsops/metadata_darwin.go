//go:build darwin

package fsops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// xattr names that carry semantics we cannot normalise in phase one. Their
// presence is detected through the attribute list, so "not implemented" never
// degrades into "assumed absent".
const (
	xattrACL          = "com.apple.acl.text"
	xattrResourceFork = "com.apple.ResourceFork"
)

const (
	maxXattrValueBytes = 1 << 20
	maxXattrTotalBytes = 4 << 20
)

// readMetadata digests every extended attribute name and value, and reports
// ACL / resource-fork presence. Any condition that prevents a complete reading
// sets Unsupported, which makes the caller keep the file.
func readXattrEvidence(fd int) (MetadataEvidence, error) {
	var ev MetadataEvidence

	sz, err := unix.Flistxattr(fd, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			ev.Unsupported = "xattr_not_supported"
			return ev, nil
		}
		return ev, fmt.Errorf("fsops: listxattr: %w", err)
	}
	if sz == 0 {
		ev.XattrDigest = emptyDigest()
		return ev, nil
	}
	buf := make([]byte, sz)
	n, err := unix.Flistxattr(fd, buf)
	if err != nil {
		return ev, fmt.Errorf("fsops: listxattr: %w", err)
	}
	names := splitNUL(buf[:n])
	sort.Strings(names)

	h := sha256.New()
	total := 0
	for _, name := range names {
		vsz, err := unix.Fgetxattr(fd, name, nil)
		if err != nil {
			switch {
			case errors.Is(err, unix.ENOATTR), errors.Is(err, unix.ENODATA):
				vsz = 0
			case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
				ev.Unsupported = "xattr_unreadable:" + name
				return ev, nil
			default:
				return ev, fmt.Errorf("fsops: getxattr size %q: %w", name, err)
			}
		}
		if vsz > maxXattrValueBytes {
			ev.Unsupported = "xattr_value_too_large:" + name
			return ev, nil
		}
		total += vsz
		if total > maxXattrTotalBytes {
			ev.Unsupported = "xattr_total_too_large"
			return ev, nil
		}

		value := make([]byte, vsz)
		if vsz > 0 {
			got, err := unix.Fgetxattr(fd, name, value)
			if err != nil {
				return ev, fmt.Errorf("fsops: getxattr %q: %w", name, err)
			}
			if got != vsz {
				// The value changed between the size probe and the read.
				ev.Unsupported = "xattr_changed:" + name
				return ev, nil
			}
			value = value[:got]
		}

		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(value)
		h.Write([]byte{0})

		switch name {
		case xattrACL:
			if len(value) > 0 {
				ev.HasACL = true
			}
		case xattrResourceFork:
			if len(value) > 0 {
				ev.HasResourceFork = true
			}
		}
		ev.XattrCount++
	}

	if ev.HasACL {
		// A non-trivial ACL is a semantic difference we cannot normalise yet.
		ev.Unsupported = "acl_present"
		return ev, nil
	}
	if ev.HasResourceFork {
		ev.Unsupported = "resource_fork_present"
		return ev, nil
	}
	ev.XattrDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return ev, nil
}

func splitNUL(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}

func emptyDigest() string {
	h := sha256.New()
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
