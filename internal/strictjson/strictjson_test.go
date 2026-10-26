package strictjson

import (
	"bytes"
	"strings"
	"testing"
)

type sample struct {
	Name  string   `json:"name"`
	Count int64    `json:"count"`
	Notes []string `json:"notes"`
}

func opts() Options {
	return Options{MaxBytes: 4096, MaxDepth: 8, IntegerOnly: true, StrictStruct: true}
}

func TestAcceptsAWellFormedDocument(t *testing.T) {
	var s sample
	raw := []byte(`{"name":"a","count":3,"notes":["x","y"]}`)
	if err := Unmarshal(raw, &s, opts()); err != nil {
		t.Fatalf("rejected a valid document: %v", err)
	}
	if s.Name != "a" || s.Count != 3 || len(s.Notes) != 2 {
		t.Fatalf("decoded wrong: %+v", s)
	}
}

func TestRejections(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"duplicate key", `{"name":"a","name":"b"}`, "duplicate"},
		{"unknown field", `{"name":"a","surprise":1}`, "unknown"},
		{"float", `{"count":1.5}`, "integer"},
		{"exponent", `{"count":1e3}`, "integer"},
		{"trailing data", `{"name":"a"}{"name":"b"}`, "trailing"},
		{"trailing garbage", `{"name":"a"} x`, "syntax"},
		{"truncated", `{"name":`, "syntax"},
		{"leading BOM", "\ufeff{\"name\":\"a\"}", "BOM"},
		{"empty", ``, "empty"},
		{"not an object", `[]`, "JSON"},
	}
	for _, c := range cases {
		var s sample
		err := Unmarshal([]byte(c.raw), &s, opts())
		if err == nil {
			t.Errorf("%s: accepted %q", c.name, c.raw)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
	}
}

func TestDepthLimit(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 20) + `1` + strings.Repeat(`}`, 20)
	var v any
	err := Unmarshal([]byte(deep), &v, Options{MaxBytes: 4096, MaxDepth: 4})
	if err == nil {
		t.Fatal("a deeply nested document was accepted")
	}
	if !strings.Contains(err.Error(), "TOO_DEEP") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSizeLimit(t *testing.T) {
	big := `{"name":"` + strings.Repeat("a", 5000) + `"}`
	var s sample
	err := Unmarshal([]byte(big), &s, Options{MaxBytes: 64})
	if err == nil {
		t.Fatal("an oversized document was accepted")
	}
}

// TestIntegerPrecisionIsNotLost guards the reason IntegerOnly exists: a float
// round-trip can turn 9007199254740993 into 9007199254740992.
func TestIntegerPrecisionIsNotLost(t *testing.T) {
	var s sample
	if err := Unmarshal([]byte(`{"count":9007199254740993}`), &s, opts()); err != nil {
		t.Fatalf("rejected a large integer: %v", err)
	}
	if s.Count != 9007199254740993 {
		t.Fatalf("precision lost: %d", s.Count)
	}
}

func TestStringNumbersAreNotCoerced(t *testing.T) {
	var s sample
	if err := Unmarshal([]byte(`{"count":"12"}`), &s, opts()); err == nil {
		t.Fatal("a quoted number was accepted as an integer")
	}
}

func TestInvalidUTF8Rejected(t *testing.T) {
	raw := []byte{'{', '"', 'n', 'a', 'm', 'e', '"', ':', '"', 0xff, '"', '}'}
	var s sample
	if err := Unmarshal(bytes.TrimSpace(raw), &s, opts()); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
}
