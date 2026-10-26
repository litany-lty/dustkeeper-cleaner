package policy

import (
	"strings"
	"testing"
)

// FuzzMatchName checks the wildcard matcher against two properties that hold
// for the documented language: a pattern with no wildcard is an equality test,
// and a trailing '*' absorbs anything.
func FuzzMatchName(f *testing.F) {
	f.Add("*.zip", "a.zip")
	f.Add("*", "")
	f.Add("?", "x")
	f.Add("a*b*c", "axxbyyc")
	f.Add("*备份*", "2024备份.tar.gz")
	f.Add("", "")
	f.Add("*.tar.gz", "a.tar.gz")
	f.Add("*a*a*a*a*a*a*a*a*b", strings.Repeat("a", 200))

	f.Fuzz(func(t *testing.T, pattern, name string) {
		got := MatchName(pattern, name)

		if !strings.ContainsAny(pattern, "*?") {
			if want := pattern == name; got != want {
				t.Fatalf("literal pattern %q against %q = %v, want %v", pattern, name, got, want)
			}
		}

		if strings.HasSuffix(pattern, "*") && strings.Count(pattern, "*") == 1 {
			// "<lit>*" matches every name that starts with <lit>.
			lit := strings.TrimSuffix(pattern, "*")
			if !strings.ContainsAny(lit, "?") {
				if want := strings.HasPrefix(name, lit); got != want {
					t.Fatalf("prefix pattern %q against %q = %v, want %v", pattern, name, got, want)
				}
			}
		}

		if !got {
			// Appending a star must not turn a non-match into a non-match of the
			// extended pattern for the same name when the name already matched
			// the prefix form; only assert the safe direction.
			return
		}
		if !MatchName(pattern+"*", name) {
			t.Fatalf("pattern %q matches %q but %q does not", pattern, name, pattern+"*")
		}
	})
}

// FuzzHasDirPrefix pins the segment-aware prefix rule to a simple oracle: for a
// prefix that ends with '/', the answer must agree with a plain string prefix
// test. Disagreement is exactly the "keep/ matches keeper/" bug.
func FuzzHasDirPrefix(f *testing.F) {
	f.Add("keep/a.zip", "keep/")
	f.Add("keeper/a.zip", "keep/")
	f.Add("keep/", "keep/")
	f.Add("a/b/c", "a/b/")
	f.Add("x", "y/")

	f.Fuzz(func(t *testing.T, rel, prefix string) {
		if !strings.HasSuffix(prefix, "/") || prefix == "/" {
			return
		}
		body := strings.TrimSuffix(prefix, "/")
		if body == "" {
			return
		}
		for _, seg := range strings.Split(body, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return
			}
		}
		if strings.ContainsAny(prefix, "\\\x00") {
			return
		}

		got := HasDirPrefix(rel, prefix)
		// The directory itself counts as covered; "keeper" must not.
		want := rel == body || strings.HasPrefix(rel, prefix)
		if got != want {
			t.Fatalf("HasDirPrefix(%q, %q) = %v, want %v", rel, prefix, got, want)
		}
	})
}
