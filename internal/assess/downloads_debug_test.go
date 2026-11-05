package assess

import (
	"testing"
	"time"
)

func TestDownloadedCredentialNamesAreNeverReviewCandidates(t *testing.T) {
	for _, name := range []string{"AccessKey.csv", "ossKey.txt"} {
		f := oldRegular(name)
		r := Assess(f, 30, time.Now())
		if r.Disposition != "keep" {
			t.Fatalf("credential-like download %q entered review: %+v", name, r)
		}
	}
}
