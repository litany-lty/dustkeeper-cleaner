package scan

// Test hook.
//
// The failed-directory branch has no portable real trigger. A test that chmods a
// directory to 000 proves nothing when it runs as root — the Linux harness runs
// privileged in a container — and macOS TCC/sandbox paths are not reproducible in
// CI. Without a seam the branch can only be reached by luck, so it would only be
// covered by the branch that never runs.
//
// The hook is nil in production, so `if hook != nil` is the entire cost. It is not
// configuration: nothing reads it from the environment, the command line or the
// state directory, and no production path assigns it.

// TestHookDirRead is consulted before a queued directory is opened. Returning a
// non-nil error makes the scanner record that directory as failed, with the error
// text as the reason, exactly as if the kernel had refused the open.
var TestHookDirRead func(rel string) error

// TestHookEntryStat is consulted before one directory entry is inspected.
// Returning a non-nil error makes the scanner see that error instead of the
// entry's metadata.
//
// The directory hook exists because mode bits prove nothing when the suite runs
// as root; this one exists for the same reason one level down. An entry whose
// stat fails with EIO or EACCES is the case that used to be indistinguishable
// from "the file is gone": recorded in one process's counter, then forgotten,
// with the epoch completing and the observation retired. It cannot be reproduced
// portably without a seam.
var TestHookEntryStat func(rel string) error
