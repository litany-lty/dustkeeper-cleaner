package policy

import "testing"

// B28: built-in protections describe dangerous categories, not an exact
// spelling. Case variants are reachable filenames and aliases on default APFS.
func TestB28GlobalProtectionCoversCaseVariants(t *testing.T) {
	for _, rel := range []string{
		"repo/.GIT/config",
		"Secrets/CERT.PEM",
		"ID_RSA",
		"Photo.PHOTOSLIBRARY/database",
	} {
		if ok, _ := GlobalProtected(rel); !ok {
			t.Errorf("%q bypassed its built-in protection through case", rel)
		}
	}
}
