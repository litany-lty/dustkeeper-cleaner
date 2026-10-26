package rules

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"dustkeeper/internal/errs"
	"dustkeeper/internal/strictjson"
)

// Draft purposes returned by the model adapter.
const (
	DraftKind           = "draft"
	DraftNeedsInputKind = "needs_input"
	DraftUnsupported    = "unsupported"
)

// MaxDraftBytes bounds one model response. A larger answer is treated as an
// error, never truncated into something that might parse.
const MaxDraftBytes = 64 << 10

// MaxInputRunes bounds the user's description.
const MaxInputRunes = 4000

// MaxQuestions is the documented ceiling for a clarification request.
const MaxQuestions = 3

// DraftRequest is what the adapter is allowed to see.
//
// It contains the user's own words and the logical scope IDs. It never contains
// scanned file names, hashes, statistics or inventory.
type DraftRequest struct {
	Text   string   `json:"text"`
	Scopes []string `json:"allowed_scopes"`
	System string   `json:"system"`
}

// Draft is the discriminated result.
type Draft struct {
	Kind      string
	DSL       string
	Questions []string
	Reason    string
	// NeedsInput is true only for the clarification variant.
	NeedsInput bool
}

// DraftProvider is the single adapter seam. Phase one has one HTTP
// implementation and a disabled mode.
type DraftProvider interface {
	Draft(ctx context.Context, req DraftRequest) ([]byte, error)
}

// envelope is the wire shape the model must return. Unknown fields are
// rejected, so a model cannot smuggle an extra "auto_purge": true alongside a
// valid draft.
type envelope struct {
	Kind string `json:"kind"`
	// DSL is kept raw so the draft body goes through the same validator as an
	// imported rule file. Binding it here would apply a second, weaker contract.
	DSL       *json.RawMessage `json:"dsl"`
	Questions []string         `json:"questions"`
	Reason    string           `json:"reason"`
}

// ParseDraft validates the model's answer under the strict contract.
//
// The three variants are mutually exclusive. A draft that also carries
// questions, an unsupported answer that carries DSL, or a response wrapped in
// prose is rejected outright: "best effort extraction" from a chatty answer is
// how an unintended rule gets activated.
func ParseDraft(raw []byte, allowedScopes map[string]bool) (Draft, error) {
	var env envelope
	if err := strictjson.Unmarshal(raw, &env, strictjson.Options{
		MaxBytes:     MaxDraftBytes,
		MaxDepth:     12,
		IntegerOnly:  true,
		StrictStruct: true,
	}); err != nil {
		return Draft{}, errs.Newf(errs.CodeDSLInvalid,
			"the model answer is not a strict JSON draft: %v", err)
	}

	// Which variant this is depends on which fields are present, not on whether
	// the ones present happen to be empty. An answer carrying `questions: []`
	// is trying to be two things at once.
	presence, err := strictjson.Inspect(raw)
	if err != nil {
		return Draft{}, errs.Newf(errs.CodeDSLInvalid,
			"the model answer is not a strict JSON draft: %v", err)
	}

	switch env.Kind {
	case DraftKind:
		if err := forbidFields(presence, "draft", "questions", "reason"); err != nil {
			return Draft{}, err
		}
		if env.DSL == nil {
			return Draft{}, errs.New(errs.CodeDSLInvalid, "a draft without a rule body")
		}
		rule, err := Parse(*env.DSL, allowedScopes)
		if err != nil {
			return Draft{}, err
		}
		canonical, err := rule.CanonicalJSON()
		if err != nil {
			return Draft{}, errs.Wrap(errs.CodeDSLInvalid, err)
		}
		return Draft{Kind: DraftKind, DSL: string(canonical)}, nil

	case DraftNeedsInputKind:
		if err := forbidFields(presence, "clarification request", "dsl", "reason"); err != nil {
			return Draft{}, err
		}
		if len(env.Questions) == 0 || len(env.Questions) > MaxQuestions {
			return Draft{}, errs.Newf(errs.CodeDSLInvalid,
				"a clarification request needs between 1 and %d questions", MaxQuestions)
		}
		for _, q := range env.Questions {
			if strings.TrimSpace(q) == "" {
				return Draft{}, errs.New(errs.CodeDSLInvalid, "an empty clarification question")
			}
		}
		return Draft{Kind: DraftNeedsInputKind, Questions: env.Questions, NeedsInput: true}, nil

	case DraftUnsupported:
		if err := forbidFields(presence, "unsupported answer", "dsl", "questions"); err != nil {
			return Draft{}, err
		}
		if strings.TrimSpace(env.Reason) == "" {
			return Draft{}, errs.New(errs.CodeDSLInvalid, "an unsupported answer needs a reason")
		}
		return Draft{Kind: DraftUnsupported, Reason: env.Reason}, nil
	}

	return Draft{}, errs.Newf(errs.CodeDSLInvalid,
		"unknown draft kind %q; expected %s, %s or %s",
		env.Kind, DraftKind, DraftNeedsInputKind, DraftUnsupported)
}

// forbidFields rejects a variant that carries a field belonging to another
// variant. Empty values count: the field being there at all is the signal.
func forbidFields(presence map[string]strictjson.Shape, what string, paths ...string) error {
	for _, path := range paths {
		if _, ok := presence[path]; ok {
			return errs.Newf(errs.CodeDSLInvalid,
				"a %s must not carry the field %q", what, path)
		}
	}
	return nil
}

// Compile runs one provider call and validates the answer.
func Compile(ctx context.Context, p DraftProvider, text string, allowedScopes map[string]bool) (Draft, error) {
	if p == nil {
		return Draft{}, errs.New(errs.CodeModelUnavailable,
			"no rule-draft model is configured; use `rule import` with a hand-written rule")
	}
	if !utf8.ValidString(text) {
		return Draft{}, errs.New(errs.CodeInvalidArgument, "the description is not valid UTF-8")
	}
	if utf8.RuneCountInString(text) > MaxInputRunes {
		return Draft{}, errs.Newf(errs.CodeInvalidArgument,
			"the description is %d characters; the limit is %d",
			utf8.RuneCountInString(text), MaxInputRunes)
	}
	scopes := make([]string, 0, len(allowedScopes))
	for id, ok := range allowedScopes {
		if ok {
			scopes = append(scopes, id)
		}
	}
	sortStrings(scopes)

	raw, err := p.Draft(ctx, DraftRequest{Text: text, Scopes: scopes, System: draftSystemPrompt})
	if err != nil {
		return Draft{}, err
	}
	if len(raw) > MaxDraftBytes {
		return Draft{}, errs.Newf(errs.CodeDSLInvalid,
			"the model answer is %d bytes; the limit is %d", len(raw), MaxDraftBytes)
	}
	return ParseDraft(raw, allowedScopes)
}

// DraftSystemPrompt returns the instructions sent with every draft request.
func DraftSystemPrompt() string { return draftSystemPrompt }

// draftSystemPrompt describes the contract in terms of the DSL, not in terms of
// examples the model could copy verbatim into an over-broad rule.
const draftSystemPrompt = `You convert one user description into exactly one Dustkeeper rule draft.
Return a single JSON object and nothing else. No markdown, no prose.

Allowed shapes:

1. A complete draft:
{"kind":"draft","dsl":{<rule>}}

2. A clarification request, when a required field is genuinely ambiguous:
{"kind":"needs_input","questions":["..."]}   (1 to 3 questions)

3. A refusal, when the request cannot be expressed safely:
{"kind":"unsupported","reason":"..."}

The <rule> object has exactly these fields:
  dsl_version            integer, always 1
  scope_id               one of the allowed scope IDs
  action                 "quarantine" only
  match                  {min_mtime_age_seconds, min_size_bytes, max_size_bytes,
                          relative_dir_prefixes, basename_patterns}
  except                 {relative_dir_prefixes, basename_patterns}
  retention_seconds      integer

Rules:
- min_mtime_age_seconds is required and must come from an explicit "not modified
  for N days" statement. Do not invent an age.
- relative_dir_prefixes end with "/" and are relative to the scope root.
- basename_patterns support only "*" and "?"; no "**", no character classes,
  no path separators.
- Do not invent a retention period. If the user did not state one, use
  "needs_input".
- Never mention absolute paths, and never propose deleting anything: a rule can
  only request quarantine.
- Nothing you output grants permission. It is a draft a human must review.`

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
