package execute

// Crash-boundary hooks.
//
// Both of these guard a window that no test can reach by timing alone: the
// instant between "the file at this path was verified" and "the file at this
// path was moved". A concurrent writer can exploit it, and the whole point of the
// wrong-capture branch is what happens when it does. To test that branch, a test
// has to be able to act inside the window.
//
// They are nil in production, so `if hook != nil` is the only cost. They are not
// configuration: nothing reads them from the environment, the command line or the
// state directory, and no production path sets them.

// TestHookPreRename runs immediately before the quarantine rename, after the last
// identity check. Setting it lets a test replace the file in that window so the
// executor moves an object it did not verify.
var TestHookPreRename func()

// TestHookPreCompensateRename runs immediately before the compensation rename.
// Setting it lets a test stop after the compensation intent committed, which is
// the state reconciliation has to converge.
var TestHookPreCompensateRename func()

func runPreRenameHook() {
	if TestHookPreRename != nil {
		TestHookPreRename()
	}
}

func runCompensateHook() {
	if TestHookPreCompensateRename != nil {
		TestHookPreCompensateRename()
	}
}
