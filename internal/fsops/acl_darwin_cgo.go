//go:build darwin && cgo

package fsops

/*
#include <sys/types.h>
#include <sys/acl.h>
#include <errno.h>

// dk_acl_probe asks the kernel for the object's extended ACL through the
// descriptor. It answers in four ways, all of which the caller needs to tell
// apart:
//
//	 1  an ACL is present and its text was copied into buf
//	 0  the object provably has no extended ACL (errno ENOENT)
//	-1  the answer cannot be established (errno is set)
//	-2  an ACL is present but its text does not fit in buf
//
// The text is a rendering of the ACL, used only as digest input and as the
// reason shown to the user. It is never parsed to decide an action.
static int dk_acl_probe(int fd, char *buf, size_t buflen, int *errout) {
	acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
	if (acl == NULL) {
		if (errno == ENOENT) {
			return 0;
		}
		*errout = errno;
		return -1;
	}
	acl_entry_t entry = NULL;
	// POSIX: acl_get_entry returns 0 on success, -1 when there is no entry.
	if (acl_get_entry(acl, ACL_FIRST_ENTRY, &entry) != 0) {
		acl_free(acl);
		return 0;
	}
	ssize_t len = (ssize_t)buflen;
	char *text = acl_to_text(acl, &len);
	if (text == NULL) {
		*errout = errno;
		acl_free(acl);
		return -1;
	}
	if (len < 0 || (size_t)len >= buflen) {
		acl_free(text);
		acl_free(acl);
		return -2;
	}
	size_t n = 0;
	while (n < (size_t)len && text[n] != 0) {
		buf[n] = text[n];
		n++;
	}
	buf[n] = 0;
	acl_free(text);
	acl_free(acl);
	return 1;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// maxACLTextBytes bounds the rendered ACL. A real ACL is far smaller; anything
// larger is reported as "too large to classify" so the file is kept, rather than
// being digested from a truncated rendering.
const maxACLTextBytes = 64 << 10

// aclEvidence reports whether the object behind fd carries a native extended
// ACL.
//
// The xattr list is not evidence about this: on APFS an ACL lives in the inode's
// attribute, not in a com.apple.* extended attribute, so an empty attribute list
// says nothing. R09 C records that the earlier code inferred "no ACL" from a
// missing xattr, which is how a copy with a deny rule could look like an
// ordinary duplicate.
func aclEvidence(fd int) (present bool, text string, reason string) {
	buf := make([]byte, maxACLTextBytes)
	var cerr C.int
	switch C.dk_acl_probe(C.int(fd), (*C.char)(unsafe.Pointer(&buf[0])), C.size_t(len(buf)), &cerr) {
	case 0:
		return false, "", ""
	case 1:
		return true, C.GoString((*C.char)(unsafe.Pointer(&buf[0]))), ""
	case -2:
		return false, "", "acl_text_too_large"
	default:
		return false, "", fmt.Sprintf("acl_undetermined_errno_%d", int(cerr))
	}
}
