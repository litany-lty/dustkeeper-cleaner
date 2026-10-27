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
