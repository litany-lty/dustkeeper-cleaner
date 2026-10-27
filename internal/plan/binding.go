// Package plan freezes candidates into an immutable, approvable plan.
//
// The immutable contract types in this file are deliberately concrete structs
// rather than map[string]any. A safety contract assembled from an untyped map
// is a contract nobody can review, and a digest over it means nothing.
package plan

import (
	"encoding/base64"
	"encoding/json"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// MetadataPolicyVersion identifies the metadata-equivalence rules used when
// deciding whether two objects are interchangeable.
const MetadataPolicyVersion = 1

// SafetyPolicyVersion identifies the global protection list generation.
const SafetyPolicyVersion = 1

// BindingVersions are written into every stored contract.
const (
	BindingVersion = 1
)

// ReviewBindingV1 binds a rule to the context a user actually reviewed.
type ReviewBindingV1 struct {
	BindingVersion           int    `json:"binding_version"`
	RuleDigest               string `json:"rule_digest"`
	ScopeID                  string `json:"scope_id"`
	ScopeGeneration          int64  `json:"scope_generation"`
	PersistentIdentityDigest string `json:"persistent_identity_digest"`
	ProtectionRevision       int64  `json:"protection_revision"`
	SafetyPolicyVersion      int    `json:"safety_policy_version"`
}

// GrantBindingV1 adds the approved action ceiling. The effective ceiling is
// always min(configuration, this value): editing the configuration cannot
// widen an approval.
type GrantBindingV1 struct {
	ReviewBindingV1
	RuleID                  string `json:"rule_id"`
	RuleRevision            int64  `json:"rule_revision"`
	Capability              string `json:"capability"`
	ApprovedMaxFiles        int    `json:"approved_max_files"`
	ApprovedMaxLogicalBytes int64  `json:"approved_max_logical_bytes"`
	BoundRetentionSeconds   int64  `json:"bound_retention_seconds"`
}

// CanonicalJSON renders a binding deterministically.
func CanonicalJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}

// Digest computes the purpose-tagged digest of a binding.
func Digest(purpose string, v any) (domain.Digest, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return "", err
	}
	return domain.DigestOf(purpose, b), nil
}

// ItemBindingV1 is stored on every item. For lifecycle items the rule
// reference and rule digest are required; for exact duplicates both are null,
// because a duplicate is authorised by a specific plan, not by a standing rule.
type ItemBindingV1 struct {
	BindingVersion            int     `json:"binding_version"`
	RuleID                    *string `json:"rule_id"`
	RuleRevision              *int64  `json:"rule_revision"`
	RuleDigest                *string `json:"rule_digest"`
	ScopeID                   string  `json:"scope_id"`
	ScopeGeneration           int64   `json:"scope_generation"`
	PersistentIdentityDigest  string  `json:"persistent_identity_digest"`
	ProtectionRevision        int64   `json:"protection_revision"`
	SafetyPolicyVersion       int     `json:"safety_policy_version"`
	SourceKind                string  `json:"source_kind"`
	AutoQuarantineGrantID     *string `json:"auto_quarantine_grant_id"`
	AutoQuarantineGrantDigest *string `json:"auto_quarantine_grant_digest"`
	AutoPurgeGrantID          *string `json:"auto_purge_grant_id"`
	AutoPurgeGrantDigest      *string `json:"auto_purge_grant_digest"`
	BoundRetentionSeconds     int64   `json:"bound_retention_seconds"`
	MetadataPolicyVersion     int     `json:"metadata_policy_version"`
}

// KeeperEvidenceV1 records the copy that must survive.
type KeeperEvidenceV1 struct {
	ScopeID        string              `json:"scope_id"`
	RawRelativeB64 string              `json:"raw_relative_b64"`
	Identity       domain.FileIdentity `json:"identity"`
	Token          domain.ChangeToken  `json:"token"`
	ContentHash    string              `json:"content_hash"`
	MetadataHash   string              `json:"metadata_hash"`
	MetadataPolicy int                 `json:"metadata_policy_version"`
}

// OperationContractV1 is written into the intent event. It records exactly what
// the executor believed before touching the file, so reconciliation can compare
// belief with reality after a crash.
type OperationContractV1 struct {
	BindingVersion  int                 `json:"binding_version"`
	ItemID          string              `json:"item_id"`
	ExecutionPlanID *string             `json:"execution_plan_id"`
	PriorState      string              `json:"prior_state"`
	NextState       string              `json:"next_state"`
	Purpose         string              `json:"purpose"`
	ScopeID         string              `json:"scope_id"`
	RawRelativeB64  string              `json:"raw_relative_b64"`
	ObjectName      string              `json:"object_name"`
	ParentIdentity  domain.FileIdentity `json:"parent_identity"`
	ExpectedIdent   domain.FileIdentity `json:"expected_identity"`
	ExpectedToken   domain.ChangeToken  `json:"expected_token"`
	ContentHash     *string             `json:"content_hash,omitempty"`
	MetadataHash    *string             `json:"metadata_hash,omitempty"`
	Binding         ItemBindingV1       `json:"binding"`
	MaxFiles        int                 `json:"max_files"`
	MaxLogicalBytes int64               `json:"max_logical_bytes"`
	MaxReadBytes    int64               `json:"max_read_bytes"`
	// DestParentIdentity records the directory the object was moved into.
	//
	// A restore and a compensation both depend on "did the destination
	// directory change under us", and both used to answer it by re-reading the
	// item row. The row describes where the object came from, not where it was
	// put, so an unrecorded destination could only be re-derived - which is
	// exactly the kind of guess reconciliation must not make.
	DestParentIdentity *domain.FileIdentity `json:"dest_parent_identity,omitempty"`
	// PriorOpID links an operation that converges an earlier one (adoption of a
	// legacy terminal hold, or a compensation) to the operation it converges.
	// The earlier operation keeps its own events untouched; history is
	// appended to, never rewritten.
	PriorOpID *string `json:"prior_op_id,omitempty"`
}

// PurgePurpose values, so reconciliation can tell a normal action from a
// compensation that must not restore purge eligibility.
const (
	PurposeNormal          = "normal"
	PurposeMiscaptureFixup = "miscapture_compensation"
	PurposeReconcileOnly   = "reconcile"
	// PurposeAdopt is kept for decoding only. Adoption was an operation that
	// converged an item left behind by earlier corruption; this build does not
	// create one, but contracts already on disk name it, so the constant stays and
	// reconciliation recognises it in order to refuse it rather than reinterpret
	// it as a restore.
	PurposeAdopt = "adopt"
)

// B64 encodes raw path bytes losslessly for JSON storage.
func B64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// UnB64 decodes path bytes.
func UnB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// DigestOfIdentity hashes an identity for binding comparison.
func DigestOfIdentity(id domain.FileIdentity) (string, error) {
	d, err := Digest("dustkeeper/identity/v1", id)
	return string(d), err
}

// PlanDoc is the exact structure the plan digest covers. Item state, pending
// operation, captured identity and timestamps are absent by design: they change
// during execution and must not invalidate the approval.
type PlanDoc struct {
	PlanID       string        `json:"plan_id"`
	Kind         string        `json:"kind"`
	CreatedAtNS  int64         `json:"created_at_ns"`
	ExpiresAtNS  int64         `json:"expires_at_ns"`
	Bindings     []PlanBinding `json:"bindings"`
	PolicyDigest string        `json:"policy_digest"`
	ConfigDigest string        `json:"config_digest"`
	Items        []PlanItemDoc `json:"items"`
}

// PlanBinding is one scope binding inside a plan.
type PlanBinding struct {
	ScopeID                  string `json:"scope_id"`
	Generation               int64  `json:"generation"`
	PersistentIdentityDigest string `json:"persistent_identity_digest"`
}

// PlanItemDoc is the immutable per-item contract.
type PlanItemDoc struct {
	Ordinal               int                 `json:"ordinal"`
	ItemID                string              `json:"item_id"`
	ScopeID               string              `json:"scope_id"`
	RawRelativeB64        string              `json:"raw_relative_b64"`
	ObjectName            string              `json:"object_name"`
	Reason                string              `json:"reason"`
	SourceKind            string              `json:"source_kind"`
	BoundRetentionSeconds int64               `json:"bound_retention_seconds"`
	RuleID                *string             `json:"rule_id"`
	RuleRevision          *int64              `json:"rule_revision"`
	RuleDigest            *string             `json:"rule_digest"`
	SourceParentIdentity  domain.FileIdentity `json:"source_parent_identity"`
	PreIdentity           domain.FileIdentity `json:"pre_identity"`
	PreToken              domain.ChangeToken  `json:"pre_token"`
	Keeper                *KeeperEvidenceV1   `json:"keeper"`
}

// ComputeDigest hashes the plan document.
func (d PlanDoc) ComputeDigest() (domain.Digest, error) {
	return Digest("dustkeeper/plan/v1", d)
}

// ComputeConfigDigest covers only the settings that change what an approved plan
// may do, not unrelated settings like the model endpoint or log location.
type ConfigDigestInput struct {
	MaxAutoQuarantineFiles int   `json:"max_auto_quarantine_files"`
	MaxAutoQuarantineBytes int64 `json:"max_auto_quarantine_bytes"`
	MaxAutoPurgeFiles      int   `json:"max_auto_purge_files"`
	MaxAutoPurgeBytes      int64 `json:"max_auto_purge_bytes"`
	RunSoftSeconds         int   `json:"run_soft_seconds"`
	RunReadBudgetBytes     int64 `json:"run_read_budget_bytes"`
	ReadRateBytesPerSecond int64 `json:"read_rate_bytes_per_second"`
	MinFreeBytes           int64 `json:"min_free_bytes"`
	MinFreePercent         int   `json:"min_free_percent"`
	MetadataPolicyVersion  int   `json:"metadata_policy_version"`
}

// ConfigDigestOf hashes the effective action configuration.
func ConfigDigestOf(in ConfigDigestInput) (domain.Digest, error) {
	return Digest("dustkeeper/config/v1", in)
}

// Digest purposes for the approval bindings. They are separate so that a review
// digest can never be replayed as a grant digest.
const (
	PurposeReviewBinding = "dustkeeper/review-binding/v1"
	PurposeGrantBinding  = "dustkeeper/grant-binding/v1"
)

// NewReviewBinding binds a rule revision to the scope state a user reviewed.
//
// It is recomputed from live state at every preview and at activation, so a
// protection added in between invalidates an approval instead of silently
// riding along.
func NewReviewBinding(scope store.Scope, ruleDigest domain.Digest) (ReviewBindingV1, domain.Digest, error) {
	persistent, err := PersistentIdentityDigest(scope)
	if err != nil {
		return ReviewBindingV1{}, "", err
	}
	rb := ReviewBindingV1{
		BindingVersion:           BindingVersion,
		RuleDigest:               string(ruleDigest),
		ScopeID:                  string(scope.ID),
		ScopeGeneration:          scope.Generation,
		PersistentIdentityDigest: persistent,
		ProtectionRevision:       scope.ProtectionRevision,
		SafetyPolicyVersion:      SafetyPolicyVersion,
	}
	d, err := Digest(PurposeReviewBinding, rb)
	if err != nil {
		return ReviewBindingV1{}, "", err
	}
	return rb, d, nil
}

// NewGrantBinding adds the approved action ceiling to a review binding.
//
// The ceiling is stored, not derived: editing the configuration later cannot
// widen what was approved, and lowering the configuration still narrows it.
func NewGrantBinding(rb ReviewBindingV1, ruleID domain.RuleID, revision int64, capability string,
	maxFiles int, maxBytes, retentionSeconds int64) (GrantBindingV1, domain.Digest, error) {

	switch capability {
	case domain.CapAutoQuarantine, domain.CapAutoPurge:
	default:
		return GrantBindingV1{}, "", errs.Newf(errs.CodeInvalidArgument,
			"capability must be %s or %s; got %q", domain.CapAutoQuarantine, domain.CapAutoPurge, capability)
	}
	if maxFiles <= 0 || maxBytes <= 0 {
		return GrantBindingV1{}, "", errs.New(errs.CodeInvalidArgument,
			"an approval with a zero file or byte limit forbids the action; raise the limit instead")
	}
	if retentionSeconds <= 0 {
		return GrantBindingV1{}, "", errs.New(errs.CodeInvalidArgument,
			"an approval must bind the rule's retention requirement")
	}
	gb := GrantBindingV1{
		ReviewBindingV1:         rb,
		RuleID:                  string(ruleID),
		RuleRevision:            revision,
		Capability:              capability,
		ApprovedMaxFiles:        maxFiles,
		ApprovedMaxLogicalBytes: maxBytes,
		BoundRetentionSeconds:   retentionSeconds,
	}
	d, err := Digest(PurposeGrantBinding, gb)
	if err != nil {
		return GrantBindingV1{}, "", err
	}
	return gb, d, nil
}

// PersistentIdentityDigest hashes the persistent half of a scope's root
// identity. The runtime half is deliberately excluded: a reboot changes it.
func PersistentIdentityDigest(scope store.Scope) (string, error) {
	var id domain.RootIdentity
	if err := json.Unmarshal([]byte(scope.RootIdentityJSON), &id); err != nil {
		return "", errs.Newf(errs.CodeStateWriteFailed,
			"scope %s identity is unreadable: %v", scope.ID, err)
	}
	d, err := Digest("dustkeeper/root-identity/v1", id.Persistent)
	if err != nil {
		return "", err
	}
	return string(d), nil
}
