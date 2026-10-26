package strictjson

import (
	"strings"
	"testing"
)

// The three spellings a bound struct cannot tell apart are the reason this file
// exists. If Inspect ever collapses them, the rule contract loses its teeth.
func TestInspectDistinguishesAbsentNullAndEmpty(t *testing.T) {
	doc := []byte(`{"a":[],"b":null,"c":{},"d":0,"e":{"f":[1,2]}}`)
	got, err := Inspect(doc)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	want := map[string]Shape{
		"a":   ShapeArray,
		"b":   ShapeNull,
		"c":   ShapeObject,
		"d":   ShapeScalar,
		"e":   ShapeObject,
		"e.f": ShapeArray,
	}
	for path, shape := range want {
		if got[path] != shape {
			t.Errorf("%s = %q, want %q", path, got[path], shape)
		}
	}
	if _, ok := got["missing"]; ok {
		t.Error("an absent field was reported as present")
	}
	if _, ok := got["a.0"]; ok {
		t.Error("array elements should not be reported as named fields")
	}
}

func TestInspectRefusesANonObjectRoot(t *testing.T) {
	for _, doc := range []string{`[]`, `null`, `42`, `"x"`} {
		if _, err := Inspect([]byte(doc)); err == nil {
			t.Errorf("Inspect accepted the non-object root %s", doc)
		}
	}
}

func TestRequireReportsTheFieldAndTheShape(t *testing.T) {
	presence, err := Inspect([]byte(`{"keep":[],"wrong":7,"nulled":null}`))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	fields := []Field{
		{Path: "keep", Only: []Shape{ShapeArray}},
		{Path: "wrong", Only: []Shape{ShapeArray}},
	}
	err = Require(presence, fields)
	if err == nil {
		t.Fatal("a scalar was accepted where an array is required")
	}
	se, ok := err.(*Error)
	if !ok || se.Kind != KindShape {
		t.Fatalf("error = %v, want a shape rejection", err)
	}

	err = Require(presence, []Field{{Path: "nulled", Only: []Shape{ShapeArray}}})
	if err == nil || !strings.Contains(err.Error(), "nulled") {
		t.Fatalf("a null field was accepted or not named: %v", err)
	}

	err = Require(presence, []Field{{Path: "absent", Only: []Shape{ShapeArray}}})
	if err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("an absent field was accepted or not named: %v", err)
	}
	se, ok = err.(*Error)
	if !ok || se.Kind != KindMissing {
		t.Fatalf("error = %v, want a missing-field rejection", err)
	}

	// An empty shape list means "any shape", so a present field passes.
	if err := Require(presence, []Field{{Path: "wrong"}}); err != nil {
		t.Fatalf("an unconstrained field was rejected: %v", err)
	}
}
