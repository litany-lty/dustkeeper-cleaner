// Package strictjson decodes JSON under the restrictions in section 6.3.
//
// json.Decoder.DisallowUnknownFields alone is not enough: it silently accepts
// duplicate object keys (last one wins), accepts floats where an integer is
// required, and accepts trailing values. A rule that was reviewed as one thing
// must not execute as another, so the checks here happen on the token stream
// before the value is bound to a struct.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Kind classifies a structural rejection.
type Kind string

// Rejection kinds.
const (
	KindTooLarge   = Kind("TOO_LARGE")
	KindNotUTF8    = Kind("NOT_UTF8")
	KindBOM        = Kind("BOM")
	KindTooDeep    = Kind("TOO_DEEP")
	KindDuplicate  = Kind("DUPLICATE_KEY")
	KindNotInteger = Kind("NOT_INTEGER")
	KindTrailing   = Kind("TRAILING_DATA")
	KindUnknown    = Kind("UNKNOWN_FIELD")
	KindSyntax     = Kind("SYNTAX")
)

// Error describes a structural rejection with enough context to fix the input.
type Error struct {
	Kind   Kind
	Detail string
	Offset int64
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Detail)
}

// Options controls the structural checks.
type Options struct {
	// MaxBytes rejects larger inputs before parsing.
	MaxBytes int
	// MaxDepth is the maximum nesting of objects and arrays.
	MaxDepth int
	// IntegerOnly rejects any number written with a fraction or exponent.
	IntegerOnly bool
	// StrictStruct enables DisallowUnknownFields on the final bind.
	StrictStruct bool
}

// Defaults mirror the limits used for configuration and rule DSL files.
func Defaults() Options {
	return Options{MaxBytes: 1 << 20, MaxDepth: 24, IntegerOnly: true, StrictStruct: true}
}

// Unmarshal validates the token stream, then binds into dst.
func Unmarshal(data []byte, dst any, opts Options) error {
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 1 << 20
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 24
	}
	if len(data) > opts.MaxBytes {
		return &Error{Kind: KindTooLarge, Detail: fmt.Sprintf("%d bytes exceeds %d", len(data), opts.MaxBytes)}
	}
	if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
		return &Error{Kind: KindBOM, Detail: "byte order mark is not allowed"}
	}
	if !utf8.Valid(data) {
		return &Error{Kind: KindNotUTF8, Detail: "input is not valid UTF-8"}
	}
	if err := scan(data, opts); err != nil {
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if opts.StrictStruct {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		return classifyBindError(err)
	}
	if dec.More() {
		return &Error{Kind: KindTrailing, Detail: "extra value after top-level document", Offset: dec.InputOffset()}
	}
	return nil
}

// scan walks the token stream checking depth, duplicate keys, number shape and
// the single-top-level-value requirement.
func scan(data []byte, opts Options) error {
	type frame struct {
		object    bool
		expectKey bool
		keys      map[string]struct{}
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var stack []*frame
	values := 0
	depth := 0

	closeValue := func() {
		if len(stack) == 0 {
			values++
			return
		}
		top := stack[len(stack)-1]
		if top.object {
			top.expectKey = true
		}
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Error{Kind: KindSyntax, Detail: err.Error(), Offset: dec.InputOffset()}
		}

		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
				if depth > opts.MaxDepth {
					return &Error{Kind: KindTooDeep, Detail: fmt.Sprintf("nesting exceeds %d", opts.MaxDepth), Offset: dec.InputOffset()}
				}
				stack = append(stack, &frame{object: t == '{', expectKey: t == '{'})
			case '}', ']':
				if len(stack) == 0 {
					return &Error{Kind: KindSyntax, Detail: "unbalanced closing delimiter", Offset: dec.InputOffset()}
				}
				stack = stack[:len(stack)-1]
				depth--
				closeValue()
			}
		case string:
			if len(stack) > 0 && stack[len(stack)-1].object && stack[len(stack)-1].expectKey {
				top := stack[len(stack)-1]
				if _, dup := top.keys[t]; dup {
					return &Error{Kind: KindDuplicate, Detail: fmt.Sprintf("duplicate object key %q", t), Offset: dec.InputOffset()}
				}
				if top.keys == nil {
					top.keys = make(map[string]struct{}, 8)
				}
				top.keys[t] = struct{}{}
				top.expectKey = false
				continue
			}
			closeValue()
		case json.Number:
			if opts.IntegerOnly {
				s := t.String()
				if strings.ContainsAny(s, ".eE") {
					return &Error{Kind: KindNotInteger, Detail: fmt.Sprintf("number %q is not a decimal integer", s), Offset: dec.InputOffset()}
				}
			}
			closeValue()
		case bool, nil:
			closeValue()
		}
	}

	if values == 0 {
		return &Error{Kind: KindSyntax, Detail: "empty document"}
	}
	if values > 1 {
		return &Error{Kind: KindTrailing, Detail: "more than one top-level value"}
	}
	return nil
}

func classifyBindError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "unknown field"):
		return &Error{Kind: KindUnknown, Detail: msg}
	case strings.Contains(msg, "cannot unmarshal") && strings.Contains(msg, "into Go value of type int"):
		return &Error{Kind: KindNotInteger, Detail: msg}
	default:
		return &Error{Kind: KindSyntax, Detail: msg}
	}
}
