package policy

import (
	"strings"

	"golang.org/x/text/unicode/norm"
)

// SpellingsMayCollide reports whether two different byte strings could name the
// same directory entry.
//
// This is deliberately *not* a normalisation of paths. Nothing returned here is
// ever opened, compared for protection or deleted: it only decides whether a
// historical slot carries an alias that a human has to resolve before a
// permanent deletion. Paths keep the exact bytes the kernel gave us, because on
// a case-sensitive volume "Keep" and "keep" are two different files and
// lower-casing them would change what the product is allowed to touch.
//
// The two spellings that can name one object are case variants (APFS and HFS+
// are case-insensitive) and Unicode normalisation variants (APFS compares names
// in a normalised form, so "Café" written as one code point and as e+U+0301 are
// one file). Both are answered by comparing the case-folded NFC forms.
func SpellingsMayCollide(a, b string) bool {
	if a == b {
		return true
	}
	return strings.EqualFold(norm.NFC.String(a), norm.NFC.String(b))
}
