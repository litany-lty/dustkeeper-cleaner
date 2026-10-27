//go:build linux

package fsops

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sys/unix"
)

// POSIX ACLs on Linux live in these attributes. Their presence in the attribute
// list is reliable detection, so an unimplemented reader never becomes "no ACL".
const (
	xattrACLAccess  = "system.posix_acl_access"
	xattrACLDefault = "system.posix_acl_default"
)

const (
	maxXattrValueBytes = 1 << 20
	maxXattrTotalBytes = 4 << 20
)

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
			case errors.Is(err, unix.ENODATA):
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
				ev.Unsupported = "xattr_changed:" + name
				return ev, nil
			}
			value = value[:got]
		}

		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(value)
		h.Write([]byte{0})

		if name == xattrACLAccess || name == xattrACLDefault {
			if len(value) > 0 {
				ev.HasACL = true
			}
		}
		ev.XattrCount++
	}

	if ev.HasACL {
		ev.Unsupported = "acl_present"
		return ev, nil
	}
	ev.XattrDigest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return ev, nil
}

// aclEvidence is satisfied on Linux by the attribute list itself: a POSIX ACL is
// stored as system.posix_acl_access (or _default), which readXattrEvidence
// already digests and classifies. There is no second, inode-resident ACL to ask
// the kernel about, so this reports "nothing beyond the attributes" rather than
// pretending to have looked somewhere else.
func aclEvidence(int) (present bool, text string, reason string) { return false, "", "" }

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
