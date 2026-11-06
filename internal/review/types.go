// Package review turns a candidate into a stated verdict: allow, insufficient
// or keep. It grants no filesystem authority — the executor's own checks still
// decide whether an action may happen — and it never fills a missing fact in.
//
// The package used to require a per-file operator statement plus a verified
// copy outside every cleanup scope before any model was asked anything. That
// made the user do the judging. What replaces it is a set of facts the program
// measures itself, labelled with where each one came from, so a reviewer can
// tell "measured" from "unknown" and answer insufficient instead of guessing.
package review

import (
	"context"
	"dustkeeper/internal/domain"
)

// Checks are the local safety facts the decision depends on. They are measured
// by the executor before any model is asked, and a false here means the action
// is already impossible: no reviewer can turn it back on.
//
// Every field is a pointer on purpose. A nil means "the program did not measure
// this", which is not the same as false, and a reviewer must be able to tell
// the two apart: a build that does not probe open files must not report
// "nothing has it open".
type Checks struct {
	Protected      *bool `json:"protected"`
	SingleLink     *bool `json:"single_link"`
	InUse          *bool `json:"in_use_by_keeper"`
	OpenByProcess  *bool `json:"open_by_visible_process"`
	KnownReference *bool `json:"known_reference"`
	ParentStable   *bool `json:"parent_unchanged"`
	ContentRead    *bool `json:"content_read"`
}

// Bool returns a pointer for a measured fact.
func Bool(v bool) *bool { return &v }

// FileObservation is bounded local metadata, not proof of global non-use.
type FileObservation struct {
	ObservedAtNS      int64  `json:"observed_at_ns,omitempty"`
	ModifiedAtNS      int64  `json:"modified_at_ns"`
	AccessedAtNS      int64  `json:"accessed_at_ns"`
	LastUsedAtNS      int64  `json:"last_used_at_ns,omitempty"`
	LastUsedStatus    string `json:"last_used_status"`
	ProcessVisibility string `json:"process_visibility"`
	ReferenceCoverage string `json:"reference_coverage"`
}

// PolicyFacts is what the user confirmed once, restated to the reviewers so
// that "disposable" is judged against a scope the user actually approved.
type PolicyFacts struct {
	DirectDeleteCategories []string `json:"direct_delete_categories"`
	ObservationDays        int      `json:"observation_days"`
}

// Facts is everything a reviewer is given. No source byte and no absolute path
// is part of it.
//
// FactsSource is deliberately prominent: it says these values were measured by
// the program, and the prompts tell the reviewer that anything absent is
// unknown rather than false.
type Facts struct {
	Action        string           `json:"action"`
	FactsSource   string           `json:"facts_source"`
	ScopeID       string           `json:"scope_id"`
	RelativePath  string           `json:"relative_path"`
	Category      string           `json:"category"`
	SizeBytes     int64            `json:"size_bytes"`
	AgeDays       int              `json:"age_days"`
	Checks        Checks           `json:"local_checks"`
	Observation   *FileObservation `json:"observation,omitempty"`
	Policy        PolicyFacts      `json:"confirmed_policy"`
	ContentSHA256 string           `json:"content_sha256,omitempty"`
}

// JEVDiagnostic records validated model output separately from the local gate.
// Scores describe the vendor's response, not a probability of safe deletion.
type JEVDiagnostic struct {
	Choice              string             `json:"choice"`
	Confidence          float64            `json:"confidence"`
	Probabilities       map[string]float64 `json:"probabilities"`
	MinConfidence       float64            `json:"min_confidence"`
	MinAllowProbability float64            `json:"min_allow_probability"`
	Blockers            []string           `json:"blockers"`
	GatePassed          bool               `json:"gate_passed"`
	Transport           string             `json:"transport"`
	Usage               *TokenUsage        `json:"usage,omitempty"`
}

type TokenUsage struct {
	OutputTokensDetails      map[string]int64 `json:"output_tokens_details,omitempty"`
	CacheReadInputTokens     int64            `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int64            `json:"cache_creation_input_tokens,omitempty"`
	InputTokens              int64            `json:"input_tokens"`
	OutputTokens             int64            `json:"output_tokens"`
}

// Decision is the reviewers' answer, normalised.
//
// Verdict is one of domain.VerdictAllow / VerdictInsufficient / VerdictKeep and
// is the only field the disposition depends on. ModelError is set when the
// review did not complete; it is never treated as an approval.
type Decision struct {
	RequestTimings  []RequestTiming `json:"request_timings,omitempty"`
	LLMExplanation  string          `json:"llm_explanation,omitempty"`
	MissingEvidence []string        `json:"missing_evidence,omitempty"`
	ModelError      string          `json:"model_error,omitempty"`
	Verdict         string          `json:"verdict"`
	LLMDecision     string          `json:"llm_decision,omitempty"`
	LLMReason       string          `json:"llm_reason,omitempty"`
	LLMUsage        *TokenUsage     `json:"llm_usage,omitempty"`
	JEV             *JEVDiagnostic  `json:"jev,omitempty"`
	Reason          string          `json:"reason"`
	JEVModel        string          `json:"jev_model"`
	LLMModel        string          `json:"llm_model"`
	// LLMConsulted distinguishes "the second reviewer agreed" from "the second
	// reviewer was never asked because the first one said keep".
	LLMConsulted bool `json:"llm_consulted"`
	// Failure is the structured reason the review produced no verdict, or empty
	// when it did. It is separate from ModelError because the sentence is for a
	// person and this value decides the control range, the exit code and
	// whether a request may already have been billed.
	Failure string `json:"failure,omitempty"`
	// Attempts counts HTTP attempts actually made for this candidate. It is
	// measured at the request site, never inferred from the verdict.
	Attempts int `json:"http_attempts"`
	// JEVSent and LLMSent say whether that provider was asked at all, so
	// "the second reviewer disagreed" cannot be confused with "it never ran".
	JEVSent bool `json:"jev_sent"`
	LLMSent bool `json:"minimax_sent"`
	// CostStatus is what is known about this candidate's provider cost:
	// not_sent, unknown, or usage_recorded (token counts arrived; the amount
	// still depends on billing evidence).
	CostStatus string `json:"cost_status"`
}

// Attempted reports whether any provider request was made for this candidate.
func (d Decision) Attempted() bool { return d.Attempts > 0 }

// NotStarted reports that the review never began, so nothing was sent and
// nothing can have been billed.
func (d Decision) NotStarted() bool {
	return d.Failure == FailureRunBudget || d.Failure == FailureBatchBudget
}

type Judge interface {
	Review(context.Context, Facts) (Decision, error)
}

// Request is what the executor knows about one candidate when it asks.
//
// The three booleans at the end are the local half of the decision. They are
// passed in rather than measured here because they are statements about this
// item's authority and movability that only the executor can make: the review
// supplies the models' answer, and the disposition combines the two.
type Request struct {
	Action                string
	ScopeID               string
	ItemID                string
	RelativePath          string
	Category              string
	AgeDays               int
	SizeBytes             int64
	Identity              domain.FileIdentity
	Observation           *FileObservation
	Checks                Checks
	Policy                PolicyFacts
	VerifiedContentSHA256 string
	// FactsComplete reports that every fact the decision needs was measured.
	FactsComplete bool
	// Movable reports that reversible isolation is provably safe here.
	Movable bool
	// Authorized reports that permanent deletion stands on a confirmed
	// category, a scope inside the confirmation and an active grant.
	Authorized bool
}

// Outcome is the result of a triage: a disposition plus the approval that binds
// it. Approval is nil for a keep decision, because there is nothing to approve.
type Outcome struct {
	Decision    Decision
	Approval    Approval
	Disposition string
	Reason      string
}

// Approval is checked outside the critical section (Verify), then checked again
// without I/O in the intent transaction (CheckIntent). Real approvals come from
// Service; tests may inject one to isolate existing lifecycle tests.
type Approval = domain.ReviewApproval
