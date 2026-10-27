//go:build darwin && !cgo

package fsops

// aclEvidence cannot be answered without the platform ACL API, which is reached
// through cgo. A build without cgo therefore reports "cannot determine" instead
// of "no ACL": the caller keeps the file, which is the only safe reading of a
// missing answer. Dustkeeper's own builds use cgo, because the SQLite driver
// requires it.
func aclEvidence(int) (present bool, text string, reason string) {
	return false, "", "acl_query_unavailable_without_cgo"
}
