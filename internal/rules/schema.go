// Package rules parses, validates and canonicalises the phase-one rule DSL.
//
// A rule carries conditions and a retention requirement. It never carries
// authority: no field in this file can grant permission to move or delete
// anything. That is what keeps "the model wrote a rule" from ever meaning "the
// model authorised a change".
package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/strictjson"
)

// Bounds from section 6.3. They are hard limits, not defaults.
const (
	minAgeSeconds     = 86400
	maxAgeSeconds     = 315360000
	maxSizeBound      = 9007199254740991
	minRetentionSecs  = 604800
	maxRetentionSecs  = 31536000
	maxMatchPrefixes  = 32
	maxExceptPrefixes = 64
	maxMatchPatterns  = 32
	maxExceptPatterns = 64
	maxPrefixBytes    = 512
	maxPatternBytes   = 128
)

// Match is the positive condition set. Every listed condition must hold.
type Match struct {
	MinMTimeAgeSeconds  int64    `json:"min_mtime_age_seconds"`
	MinSizeBytes        int64    `json:"min_size_bytes"`
	MaxSizeBytes        *int64   `json:"max_size_bytes"`
	RelativeDirPrefixes []string `json:"relative_dir_prefixes"`
	BasenamePatterns    []string `json:"basename_patterns"`
}

// Except is the exclusion set. Exclusions are evaluated before matches.
type Except struct {
	RelativeDirPrefixes []string `json:"relative_dir_prefixes"`
	BasenamePatterns    []string `json:"basename_patterns"`
}

// Rule is one fully validated rule revision.
type Rule struct {
	DSLVersion       int    `json:"dsl_version"`
	ScopeID          string `json:"scope_id"`
	Action           string `json:"action"`
	Match            Match  `json:"match"`
	Except           Except `json:"except"`
	RetentionSeconds int64  `json:"retention_seconds"`
}

// Parse validates raw JSON under the strict contract and returns a Rule.
//
// allowedScopes is the set of logical scope IDs the caller may reference. A
// rule can never introduce an absolute path or an unregistered scope.
func Parse(data []byte, allowedScopes map[string]bool) (*Rule, error) {
	var r Rule
	if err := strictjson.Unmarshal(data, &r, strictjson.Options{
		MaxBytes:     64 << 10,
		MaxDepth:     6,
		IntegerOnly:  true,
		StrictStruct: true,
	}); err != nil {
		return nil, mapStrict(err)
	}
	// The field contract is checked against the document, not against the bound
	// struct: binding erases the difference between a missing field, a null field
	// and an empty list, and those three mean different things here.
	presence, err := strictjson.Inspect(data)
	if err != nil {
		return nil, mapStrict(err)
	}
	if err := checkRuleFields(presence); err != nil {
		return nil, err
	}
	// Normalise nil slices so canonical output always uses [] rather than null.
	if r.Match.RelativeDirPrefixes == nil {
		r.Match.RelativeDirPrefixes = []string{}
	}
	if r.Match.BasenamePatterns == nil {
		r.Match.BasenamePatterns = []string{}
	}
	if r.Except.RelativeDirPrefixes == nil {
		r.Except.RelativeDirPrefixes = []string{}
	}
	if r.Except.BasenamePatterns == nil {
		r.Except.BasenamePatterns = []string{}
	}

	if err := r.Validate(allowedScopes); err != nil {
		return nil, err
	}
	return &r, nil
}

// ruleFields is the documented field contract. Every field the DSL lists must be
// present, and every field may only hold the shapes listed here.
//
// max_size_bytes is the single field whose *value* may be null. That is not the
// same as "may be absent": the DSL has one spelling for "no upper bound", and it
// is `null`. A missing field is a model that dropped the ceiling, and accepting
// it would turn "I forgot the limit" into "there is no limit".
var ruleFields = []strictjson.Field{
	{Path: "dsl_version", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	{Path: "scope_id", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	{Path: "action", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	{Path: "match", Only: []strictjson.Shape{strictjson.ShapeObject}},
	{Path: "match.min_mtime_age_seconds", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	{Path: "match.min_size_bytes", Only: []strictjson.Shape{strictjson.ShapeScalar}},
	{Path: "match.max_size_bytes", Only: []strictjson.Shape{strictjson.ShapeScalar, strictjson.ShapeNull}},
	{Path: "match.relative_dir_prefixes", Only: []strictjson.Shape{strictjson.ShapeArray}},
	{Path: "match.basename_patterns", Only: []strictjson.Shape{strictjson.ShapeArray}},
	{Path: "except", Only: []strictjson.Shape{strictjson.ShapeObject}},
	{Path: "except.relative_dir_prefixes", Only: []strictjson.Shape{strictjson.ShapeArray}},
	{Path: "except.basename_patterns", Only: []strictjson.Shape{strictjson.ShapeArray}},
	{Path: "retention_seconds", Only: []strictjson.Shape{strictjson.ShapeScalar}},
}

// checkRuleFields enforces the table above: every listed field is present with
// one of its documented shapes, and nothing else is present at all.
func checkRuleFields(presence map[string]strictjson.Shape) error {
	if err := strictjson.Require(presence, ruleFields); err != nil {
		return mapStrict(err)
	}
	// Require answers "is what has to be here, here". It says nothing about a key
	// that should not be here, and encoding/json binds field names
	// case-insensitively: {"scope_id": "a", "SCOPE_ID": "b"} would pass Require and
	// then execute the second value. The exact key set closes that, and it is the
	// same table, so presence and binding cannot drift apart.
	if err := strictjson.Allow(presence, ruleFields); err != nil {
		return mapStrict(err)
	}
	return nil
}

func mapStrict(err error) error {
	se, ok := err.(*strictjson.Error)
	if !ok {
		return errs.Newf(errs.CodeDSLInvalid, "%v", err)
	}
	switch se.Kind {
	case strictjson.KindMissing, strictjson.KindShape:
		return errs.Newf(errs.CodeDSLInvalid, "%s", se.Detail)
	case strictjson.KindUnknown:
		return errs.Newf(errs.CodeUnknownField, "%s", se.Detail)
	case strictjson.KindDuplicate:
		return errs.Newf(errs.CodeDuplicateKey, "%s", se.Detail)
	case strictjson.KindNotInteger:
		return errs.Newf(errs.CodeDSLInvalid, "%s", se.Detail)
	case strictjson.KindTooDeep, strictjson.KindTooLarge:
		return errs.Newf(errs.CodeDSLInvalid, "%s", se.Detail)
	default:
		return errs.Newf(errs.CodeDSLInvalid, "%s", se.Detail)
	}
}

// Validate enforces every bound in the field contract.
func (r *Rule) Validate(allowedScopes map[string]bool) error {
	if r.DSLVersion != 1 {
		return errs.Newf(errs.CodeDSLInvalid, "dsl_version must be 1, got %d", r.DSLVersion)
	}
	if !isScopeID(r.ScopeID) {
		return errs.Newf(errs.CodeDSLInvalid, "scope_id %q is not a valid scope identifier", r.ScopeID)
	}
	if allowedScopes != nil && !allowedScopes[r.ScopeID] {
		return errs.Newf(errs.CodeDSLInvalid, "scope_id %q is not a registered scope", r.ScopeID)
	}
	if r.Action != "quarantine" {
		return errs.Newf(errs.CodeRuleUnsupported, "action must be quarantine, got %q", r.Action)
	}

	if r.Match.MinMTimeAgeSeconds < minAgeSeconds || r.Match.MinMTimeAgeSeconds > maxAgeSeconds {
		return errs.Newf(errs.CodeDSLInvalid,
			"match.min_mtime_age_seconds must be in %d..%d", minAgeSeconds, maxAgeSeconds)
	}
	if r.Match.MinSizeBytes < 0 || r.Match.MinSizeBytes > maxSizeBound {
		return errs.Newf(errs.CodeDSLInvalid, "match.min_size_bytes must be in 0..%d", maxSizeBound)
	}
	if r.Match.MaxSizeBytes != nil {
		if *r.Match.MaxSizeBytes < 0 || *r.Match.MaxSizeBytes > maxSizeBound {
			return errs.Newf(errs.CodeDSLInvalid, "match.max_size_bytes must be in 0..%d", maxSizeBound)
		}
		if *r.Match.MaxSizeBytes < r.Match.MinSizeBytes {
			return errs.New(errs.CodeDSLInvalid, "match.max_size_bytes must not be smaller than min_size_bytes")
		}
	}

	if err := validatePrefixes(r.Match.RelativeDirPrefixes, maxMatchPrefixes, "match.relative_dir_prefixes"); err != nil {
		return err
	}
	if err := validatePrefixes(r.Except.RelativeDirPrefixes, maxExceptPrefixes, "except.relative_dir_prefixes"); err != nil {
		return err
	}
	if err := validatePatterns(r.Match.BasenamePatterns, maxMatchPatterns, "match.basename_patterns"); err != nil {
		return err
	}
	if err := validatePatterns(r.Except.BasenamePatterns, maxExceptPatterns, "except.basename_patterns"); err != nil {
		return err
	}

	if r.RetentionSeconds < minRetentionSecs || r.RetentionSeconds > maxRetentionSecs {
		return errs.Newf(errs.CodeDSLInvalid,
			"retention_seconds must be in %d..%d", minRetentionSecs, maxRetentionSecs)
	}
	if r.RetentionSeconds%86400 != 0 {
		return errs.New(errs.CodeDSLInvalid, "retention_seconds must be a whole number of days")
	}
	return nil
}

func validatePrefixes(list []string, max int, field string) error {
	if len(list) > max {
		return errs.Newf(errs.CodeDSLInvalid, "%s has %d entries, at most %d allowed", field, len(list), max)
	}
	seen := make(map[string]bool, len(list))
	for _, p := range list {
		if seen[p] {
			return errs.Newf(errs.CodeDSLInvalid, "%s contains duplicate entry %q", field, p)
		}
		seen[p] = true
		if err := validatePrefix(p); err != nil {
			return errs.Newf(errs.CodeDSLInvalid, "%s: %v", field, err)
		}
	}
	return nil
}

// validatePrefix checks one relative directory prefix. A trailing slash is
// required and is not treated as an empty segment; an empty segment anywhere
// else is rejected.
func validatePrefix(p string) error {
	if p == "" {
		return fmt.Errorf("empty prefix")
	}
	if len(p) > maxPrefixBytes {
		return fmt.Errorf("prefix longer than %d bytes", maxPrefixBytes)
	}
	if !strings.HasSuffix(p, "/") {
		return fmt.Errorf("prefix %q must end with /", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("prefix %q must be relative", p)
	}
	body := strings.TrimSuffix(p, "/")
	if body == "" {
		return fmt.Errorf("prefix %q has no path segment", p)
	}
	for _, seg := range strings.Split(body, "/") {
		if seg == "" {
			return fmt.Errorf("prefix %q has an empty segment", p)
		}
		if seg == "." || seg == ".." {
			return fmt.Errorf("prefix %q contains a relative segment", p)
		}
	}
	if strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("prefix %q contains a backslash or NUL", p)
	}
	if strings.ContainsAny(p, "*?[]{}") {
		return fmt.Errorf("prefix %q contains a wildcard or class", p)
	}
	return nil
}

func validatePatterns(list []string, max int, field string) error {
	if len(list) > max {
		return errs.Newf(errs.CodeDSLInvalid, "%s has %d entries, at most %d allowed", field, len(list), max)
	}
	seen := make(map[string]bool, len(list))
	for _, p := range list {
		if seen[p] {
			return errs.Newf(errs.CodeDSLInvalid, "%s contains duplicate entry %q", field, p)
		}
		seen[p] = true
		if err := ValidatePattern(p); err != nil {
			return errs.Newf(errs.CodeDSLInvalid, "%s: %v", field, err)
		}
	}
	return nil
}

// ValidatePattern checks the deliberately tiny pattern language: literals, '*'
// and '?'. No character classes, no brace expansion, no '**', no escaping and no
// separators. Anything richer would need a spec this phase has not written.
func ValidatePattern(p string) error {
	if p == "" {
		return fmt.Errorf("empty pattern")
	}
	if len(p) > maxPatternBytes {
		return fmt.Errorf("pattern longer than %d bytes", maxPatternBytes)
	}
	if strings.Contains(p, "**") {
		return fmt.Errorf("pattern %q contains **", p)
	}
	if strings.ContainsAny(p, "/\\\x00") {
		return fmt.Errorf("pattern %q contains a separator, backslash or NUL", p)
	}
	if strings.ContainsAny(p, "[]{}!^\\") {
		return fmt.Errorf("pattern %q contains a character class or escape", p)
	}
	return nil
}

func isScopeID(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// CanonicalJSON returns the exact byte form that the digest covers: the field
// order of the struct, no HTML escaping, no trailing newline, and sets sorted.
func (r *Rule) CanonicalJSON() ([]byte, error) {
	cp := *r
	cp.Match.RelativeDirPrefixes = sortedCopy(r.Match.RelativeDirPrefixes)
	cp.Match.BasenamePatterns = sortedCopy(r.Match.BasenamePatterns)
	cp.Except.RelativeDirPrefixes = sortedCopy(r.Except.RelativeDirPrefixes)
	cp.Except.BasenamePatterns = sortedCopy(r.Except.BasenamePatterns)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(&cp); err != nil {
		return nil, err
	}
	// Remove exactly the one trailing newline the encoder adds. Trimming all
	// whitespace would silently accept a different canonical input.
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Digest returns the rule content digest with the rule purpose tag.
func (r *Rule) Digest() (domain.Digest, error) {
	canon, err := r.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return domain.DigestOf("dustkeeper/rule/v1", canon), nil
}

// Explain renders the validated DSL as host-generated prose. The model's own
// description is never used as approval text: only this, derived from the
// validated structure, is shown to the user.
func (r *Rule) Explain() []string {
	var out []string
	out = append(out, fmt.Sprintf("scope: %s", r.ScopeID))

	var conds []string
	conds = append(conds, fmt.Sprintf("not modified for at least %s", humanDuration(r.Match.MinMTimeAgeSeconds)))
	if len(r.Match.BasenamePatterns) > 0 {
		conds = append(conds, fmt.Sprintf("name matches any of %s", strings.Join(r.Match.BasenamePatterns, ", ")))
	} else {
		conds = append(conds, "any file name")
	}
	if len(r.Match.RelativeDirPrefixes) > 0 {
		conds = append(conds, fmt.Sprintf("under %s", strings.Join(r.Match.RelativeDirPrefixes, ", ")))
	} else {
		conds = append(conds, "anywhere in the scope")
	}
	conds = append(conds, sizeText(r.Match.MinSizeBytes, r.Match.MaxSizeBytes))
	out = append(out, "match: "+strings.Join(conds, "; "))

	if len(r.Except.RelativeDirPrefixes) > 0 {
		out = append(out, "exclude directories: "+strings.Join(r.Except.RelativeDirPrefixes, ", "))
	}
	if len(r.Except.BasenamePatterns) > 0 {
		out = append(out, "exclude names: "+strings.Join(r.Except.BasenamePatterns, ", "))
	}
	out = append(out, "retention: "+humanDuration(r.RetentionSeconds)+" (before the object may be purged at all)")
	out = append(out, "this rule only selects candidates; moving files requires an explicit grant")
	return out
}

func sizeText(min int64, max *int64) string {
	switch {
	case min == 0 && max == nil:
		return "any size"
	case max == nil:
		return fmt.Sprintf("at least %d bytes", min)
	default:
		return fmt.Sprintf("%d..%d bytes", min, *max)
	}
}

func humanDuration(sec int64) string {
	days := sec / 86400
	switch {
	case days >= 1 && days%1 == 0:
		return fmt.Sprintf("%d days (%d s)", days, sec)
	default:
		return fmt.Sprintf("%d seconds", sec)
	}
}
