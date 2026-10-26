package fsops

import (
	"errors"
	"strings"
	"testing"
)

// FuzzSplitRelative pins the path validator to its accepted set: a successful
// split must rejoin to exactly the input, and every component must be a name the
// kernel will accept as one entry.
func FuzzSplitRelative(f *testing.F) {
	f.Add([]byte("a/b/c"))
	f.Add([]byte("a"))
	f.Add([]byte(""))
	f.Add([]byte("/abs"))
	f.Add([]byte("a//b"))
	f.Add([]byte("a/./b"))
	f.Add([]byte("../x"))
	f.Add([]byte("a/b/"))
	f.Add([]byte(".."))
	f.Add([]byte("a\x00b"))
	f.Add([]byte(strings.Repeat("a/", 200) + "b"))

	f.Fuzz(func(t *testing.T, rel []byte) {
		parts, err := SplitRelative(rel)
		if err != nil {
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("rejection did not wrap ErrInvalidPath: %v", err)
			}
			return
		}
		if len(parts) == 0 {
			t.Fatal("an accepted path has no components")
		}
		if got := strings.Join(parts, "/"); got != string(rel) {
			t.Fatalf("rejoin = %q, input = %q", got, string(rel))
		}
		for _, p := range parts {
			if err := ValidateName(p); err != nil {
				t.Fatalf("component %q of %q is not a valid name: %v", p, string(rel), err)
			}
		}
	})
}

// FuzzValidateName checks that anything the validator accepts can be handed to a
// renameat/openat call as a single component.
func FuzzValidateName(f *testing.F) {
	f.Add("a.zip")
	f.Add("")
	f.Add(".")
	f.Add("..")
	f.Add("a/b")
	f.Add("a\x00b")
	f.Add(strings.Repeat("x", 255))
	f.Add(strings.Repeat("x", 256))

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateName(name)
		if err == nil {
			if name == "" || name == "." || name == ".." {
				t.Fatalf("accepted the bare name %q", name)
			}
			if strings.ContainsAny(name, "/\x00") {
				t.Fatalf("accepted a name with a separator or NUL: %q", name)
			}
			if len(name) > 255 {
				t.Fatalf("accepted a name of %d bytes", len(name))
			}
			return
		}
		if !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("rejection did not wrap ErrInvalidPath: %v", err)
		}
	})
}
