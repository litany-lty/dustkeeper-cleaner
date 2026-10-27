package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Shape is what kind of JSON value a field holds. It exists because binding a
// document into a struct erases the difference between "the field was absent",
// "the field was null" and "the field was an empty array" — all three arrive as
// the zero value. A rule that was reviewed with an exclusion list must not
// execute with no exclusion list, so presence has to be checked on the document,
// not on the struct.
type Shape string

// Shapes, in the order of how much they say.
const (
	ShapeNull   = Shape("null")
	ShapeArray  = Shape("array")
	ShapeObject = Shape("object")
	ShapeScalar = Shape("scalar")
)

// Inspect reports every object member of the document as "a.b.c" → Shape.
//
// Only object members are listed: array elements have no stable name to report,
// and a rule that needs to constrain them constrains the array itself. The root
// value must be an object, since every document in this project is one.
//
// Inspect assumes the document already passed Unmarshal (syntax, size, depth,
// duplicate keys). It is a second look at the same bytes, not a substitute for
// the first.
func Inspect(data []byte) (map[string]Shape, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, &Error{Kind: KindSyntax, Detail: err.Error()}
	}
	if _, ok := root.(map[string]any); !ok {
		return nil, &Error{Kind: KindSyntax, Detail: "the top-level value is not an object"}
	}
	out := make(map[string]Shape, 16)
	walkObject("", root.(map[string]any), out)
	return out, nil
}

func walkObject(prefix string, obj map[string]any, out map[string]Shape) {
	for key, value := range obj {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		out[path] = shapeOf(value)
		if nested, ok := value.(map[string]any); ok {
			walkObject(path, nested, out)
		}
	}
}

func shapeOf(v any) Shape {
	switch v.(type) {
	case nil:
		return ShapeNull
	case []any:
		return ShapeArray
	case map[string]any:
		return ShapeObject
	default:
		return ShapeScalar
	}
}

// Require checks a required-field table against the document.
//
// It returns the first problem in field order, so the message a user sees is
// about the field they most likely dropped rather than about whichever key
// happened to iterate first.
func Require(presence map[string]Shape, fields []Field) error {
	for _, f := range fields {
		got, ok := presence[f.Path]
		if !ok {
			return &Error{Kind: KindMissing, Detail: fmt.Sprintf("field %q is required", f.Path)}
		}
		if len(f.Only) == 0 {
			continue
		}
		allowed := false
		for _, want := range f.Only {
			if got == want {
				allowed = true
				break
			}
		}
		if !allowed {
			return &Error{
				Kind:   KindShape,
				Detail: fmt.Sprintf("field %q must be %s, got %s", f.Path, joinShapes(f.Only), got),
			}
		}
	}
	return nil
}

// Field is one entry of a required-field table. Only lists the shapes the field
// may take; an empty list means "any shape".
type Field struct {
	Path string
	Only []Shape
}

// Allow rejects every member that the table does not name, using the *exact*
// spelling the table uses.
//
// Require answers "the field I need is present". This answers "the field you sent
// is one I know", and the two are not the same question because encoding/json
// binds field names case-insensitively: a document carrying both "scope_id" and
// "SCOPE_ID" satisfies Require, binds twice, and the second value wins. The same
// hole let a model emit "Questions" instead of "questions": the struct bound it,
// and a presence check that only looked for the lower-case spelling never saw it.
//
// The table is the same one Require uses, so presence and binding cannot describe
// different documents.
//
// A path whose ancestor is named in the table and whose children the table does
// not describe is allowed: "dsl" is an opaque object here, and its contents are
// checked by the DSL's own contract rather than by this table.
func Allow(presence map[string]Shape, fields []Field) error {
	allowed := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		allowed[f.Path] = struct{}{}
	}
	// Sorted so the message is about the same field on every run; map order is
	// not something a user should have to notice.
	paths := make([]string, 0, len(presence))
	for path := range presence {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		if _, ok := allowed[path]; ok {
			continue
		}
		if opaqueParent(path, allowed) {
			continue
		}
		return &Error{
			Kind:   KindUnknown,
			Detail: fmt.Sprintf("field %q is not part of this contract (names are case-sensitive)", path),
		}
	}
	return nil
}

// opaqueParent reports whether path lives inside an object the table names but
// does not describe.
func opaqueParent(path string, allowed map[string]struct{}) bool {
	for i := len(path) - 1; i > 0; i-- {
		if path[i] != '.' {
			continue
		}
		parent := path[:i]
		if _, ok := allowed[parent]; !ok {
			continue
		}
		// The parent is named. Is any of its children described?
		prefix := parent + "."
		described := false
		for candidate := range allowed {
			if len(candidate) > len(prefix) && candidate[:len(prefix)] == prefix {
				described = true
				break
			}
		}
		if !described {
			return true
		}
	}
	return false
}

func joinShapes(shapes []Shape) string {
	out := ""
	for i, s := range shapes {
		if i > 0 {
			out += " or "
		}
		out += string(s)
	}
	return out
}
