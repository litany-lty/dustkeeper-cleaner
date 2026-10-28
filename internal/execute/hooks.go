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

// TestHookDuringCompare runs once inside CompareFiles, after the first block pair
// has been compared. It exists because a test cannot otherwise reach the window
// R09 A describes: the comparison has already read a region, an older write
// descriptor changes that region, and the rest of the comparison still finds the
// files equal. It clears itself after the first call so a comparison that runs
// several times fires it once.
var TestHookDuringCompare func()

// The two acceptance hooks bracket the window between "the file the plan
// described was verified" and "the intent that acts on it committed". That
// window is where a writer can touch the file after the check and before the
// move, so a test that wants to prove the refusal has to act inside it.
//
// They clear themselves after the first call: a plan may hold several items, and
// a test that set the hook once is describing one window, not every item.

// TestHookBeforeStableRead runs between the comparison with the plan's recorded
// identity and the stable read that produces the evidence for the manifest.
var TestHookBeforeStableRead func()

// TestHookBeforeAcceptance runs after the preparation manifest was written and
// immediately before the last checks in front of the intent commit.
var TestHookBeforeAcceptance func()

// TestHookManifestDirSync fails the directory flush that follows the manifest
// install. It is fault injection, not a crash boundary: the only way to reach
// that branch is to make the flush fail, and it fails for nobody in particular.
// Returning nil leaves the real flush to run.
var TestHookManifestDirSync func() error

func runBeforeStableReadHook() {
	if f := TestHookBeforeStableRead; f != nil {
		TestHookBeforeStableRead = nil
		f()
	}
}

func runBeforeAcceptanceHook() {
	if f := TestHookBeforeAcceptance; f != nil {
		TestHookBeforeAcceptance = nil
		f()
	}
}

func runDuringCompareHook() {
	if f := TestHookDuringCompare; f != nil {
		TestHookDuringCompare = nil
		f()
	}
}

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
