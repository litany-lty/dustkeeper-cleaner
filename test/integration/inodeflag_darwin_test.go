//go:build darwin

package integration

import "testing"

// markInodeFlag has no macOS counterpart that is worth setting here: the BSD
// flags live in the ordinary stat structure, so the scanner and the executor
// read the same field and the defect this test targets cannot exist. The skip
// states that, rather than pretending the positive case ran.
func markInodeFlag(t *testing.T, path string) uint64 {
	t.Helper()
	t.Skip("macOS carries file flags in stat, so planning and verification cannot disagree on them")
	return 0
}
