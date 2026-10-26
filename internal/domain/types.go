// Package domain holds the shared identifiers, file identity, rule, plan and
// object-state types that every other package agrees on.
//
// Two rules matter for correctness and are enforced by construction:
//
//  1. Unavailable identity facts are represented by a nil pointer, never by a
//     zero value. A zero inode or a zero birthtime is indistinguishable from a
//     real one, and the reconciliation matrix depends on telling them apart.
//  2. Paths that the program will execute against are raw bytes. Display
//     strings are for humans only and are never re-encoded into a syscall.
package domain

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// Identifier types. They are distinct so that a plan ID cannot be passed where
// an item ID is required.
type (
	ScopeID      string
	PlanID       string
	ItemID       string
	OperationID  string
	RuleID       string
	GrantID      string
	ProtectionID string
	EventID      string
	Digest       string
)

// RelativePath is the raw byte form of a path relative to a scope root. It is
// never a display string and never contains a leading separator.
type RelativePath []byte

// Display renders the path for humans, replacing invalid UTF-8 so that a report
// stays printable. Never feed the result back into a syscall.
func (p RelativePath) Display() string {
	if len(p) == 0 {
		return "."
	}
	return string(bytes.ToValidUTF8(p, []byte("\uFFFD")))
}

// Scope kinds (section 2.1).
const (
	KindObserve           = "observe"
	KindAssisted          = "assisted"
	KindManagedDisposable = "managed_disposable"
	KindReference         = "reference"
)

// MutableKind reports whether a scope kind may ever change business files.
// observe and reference must never be moved or unlinked by this program.
func MutableKind(kind string) bool {
	return kind == KindAssisted || kind == KindManagedDisposable
}

// ScopeStatus values.
const (
	ScopeActive          = "active"
	ScopePaused          = "paused"
	ScopeNeedsReapproval = "needs_reapproval"
	ScopeHeld            = "held"
)

// Rule status values.
const (
	RuleDraft    = "draft"
	RuleActive   = "active"
	RuleDisabled = "disabled"
)

// Capabilities. They are granted independently.
const (
	CapAutoQuarantine = "auto_quarantine"
	CapAutoPurge      = "auto_purge"
)

// Grant status values.
const (
	GrantActive          = "active"
	GrantRevoked         = "revoked"
	GrantNeedsReapproval = "needs_reapproval"
)

// Plan kinds.
const (
	PlanManualRule      = "manual_rule"
	PlanManualDuplicate = "manual_duplicate"
	PlanScheduledRule   = "scheduled_rule"
	PlanManualPurge     = "manual_purge"
	PlanScheduledPurge  = "scheduled_purge"
)

// Plan status values.
const (
	PlanReady    = "ready"
	PlanApplying = "applying"
	PlanComplete = "complete"
	PlanPartial  = "partial"
	PlanExpired  = "expired"
	PlanBlocked  = "blocked"
)

// Item lifecycle states (section 10.1).
const (
	StatePlanned     = "planned"
	StateQIntent     = "q_intent"
	StateQuarantined = "quarantined"
	StateRIntent     = "r_intent"
	StateRestored    = "restored"
	StatePIntent     = "p_intent"
	StatePurged      = "purged"
	StateAborted     = "aborted"
	StateHeld        = "held"
	StateUnknown     = "unknown"
)

// Source kinds for items.
const (
	SourceManualRule      = "manual_rule"
	SourceManualDuplicate = "manual_duplicate"
	SourceScheduledRule   = "scheduled_rule"
)

// Reason values recorded on items.
const (
	ReasonLifecycle      = "lifecycle"
	ReasonExactDuplicate = "exact_duplicate"
)

// Event phases.
//
// PhasePark is deliberately non-terminal: it records "the namespace changed but
// the outcome is not proven yet" and leaves the operation open so reconciliation
// can still finish it. PhaseHold is the terminal verdict and is unique per
// operation.
const (
	PhaseIntent   = "intent"
	PhaseComplete = "complete"
	PhaseAbort    = "abort"
	PhaseHold     = "hold"
	PhasePark     = "park"
	PhaseInfo     = "info"
)

// Protection match kinds and sources.
const (
	MatchExact   = "exact"
	MatchSubtree = "subtree"

	ProtectUser             = "user"
	ProtectRestore          = "restore"
	ProtectEmergencyRestore = "emergency_restore"
)

// Observation status values.
const (
	ObsSeen     = "seen"
	ObsStale    = "stale"
	ObsUnusable = "unusable"
)

// RuleRef pins a rule revision and its content digest.
type RuleRef struct {
	ID       RuleID `json:"rule_id"`
	Revision int64  `json:"revision"`
	Digest   Digest `json:"digest"`
}

// PersistentRootIdentity is the part of a root identity that must survive a
// reboot. A missing fact is nil, not zero.
type PersistentRootIdentity struct {
	VolumeID      string  `json:"volume_id"`
	Inode         uint64  `json:"inode"`
	BirthTimeNS   *int64  `json:"birth_time_ns"`
	Generation    *uint64 `json:"generation"`
	OwnerUID      uint32  `json:"owner_uid"`
	FileSystem    string  `json:"filesystem"`
	VaultMarkerID string  `json:"vault_marker_id"`
}

// RuntimeMountIdentity is only valid for the current boot/run generation. Linux
// mount IDs may be reused, so this can never serve as cross-restart evidence.
type RuntimeMountIdentity struct {
	BootOrRunID string  `json:"boot_or_run_id"`
	Device      uint64  `json:"device"`
	MountID     *uint64 `json:"mount_id"`
}

// RootIdentity is the two-layer identity of a registered scope root.
type RootIdentity struct {
	Persistent PersistentRootIdentity `json:"persistent"`
	Runtime    RuntimeMountIdentity   `json:"runtime"`
}

// FileIdentity identifies one object. Device is included so that equal inode
// numbers on different volumes never look like the same object.
type FileIdentity struct {
	VolumeID    string  `json:"volume_id"`
	Device      uint64  `json:"device"`
	Inode       uint64  `json:"inode"`
	Kind        string  `json:"kind"`
	BirthTimeNS *int64  `json:"birth_time_ns"`
	Generation  *uint64 `json:"generation"`
}

// Equal compares the fields that identify an object. Birth time and generation
// are compared only when both sides have them, because a filesystem may stop
// reporting a fact that it reported earlier.
func (a FileIdentity) Equal(b FileIdentity) bool {
	if a.Device != b.Device || a.Inode != b.Inode || a.Kind != b.Kind {
		return false
	}
	if a.VolumeID != "" && b.VolumeID != "" && a.VolumeID != b.VolumeID {
		return false
	}
	if a.BirthTimeNS != nil && b.BirthTimeNS != nil && *a.BirthTimeNS != *b.BirthTimeNS {
		return false
	}
	if a.Generation != nil && b.Generation != nil && *a.Generation != *b.Generation {
		return false
	}
	return true
}

// IsZero reports whether the identity carries no evidence at all.
func (a FileIdentity) IsZero() bool { return a.Inode == 0 && a.Device == 0 && a.Kind == "" }

// ChangeToken detects that the object behind a path changed. atime is excluded:
// reading a file to hash it changes it.
type ChangeToken struct {
	Size         int64  `json:"size"`
	MTimeNS      int64  `json:"mtime_ns"`
	CTimeNS      int64  `json:"ctime_ns"`
	NLink        uint64 `json:"nlink"`
	Mode         uint32 `json:"mode"`
	UID          uint32 `json:"uid"`
	GID          uint32 `json:"gid"`
	Flags        uint64 `json:"flags"`
	MetadataHash Digest `json:"metadata_hash,omitempty"`
}

// SameContent compares the fields that must match for content identity. ctime
// is excluded because a rename may change it.
func (a ChangeToken) SameContent(b ChangeToken) bool {
	return a.Size == b.Size && a.MTimeNS == b.MTimeNS
}

// Observation is one indexed path from a scan.
type Observation struct {
	Scope       ScopeID      `json:"scope_id"`
	Relative    RelativePath `json:"-"`
	Parent      FileIdentity `json:"parent"`
	Identity    FileIdentity `json:"identity"`
	Token       ChangeToken  `json:"token"`
	FirstSeenNS int64        `json:"first_seen_ns"`
	SeenAtNS    int64        `json:"seen_at_ns"`
	ReadStatus  string       `json:"read_status"`
}

// ScopeBinding pins a scope generation and root identity into a plan.
type ScopeBinding struct {
	Scope      ScopeID      `json:"scope_id"`
	Generation int64        `json:"generation"`
	Root       RootIdentity `json:"root"`
}

// PlanRequest is what a caller asks the planner to freeze.
type PlanRequest struct {
	Kind   string   `json:"kind"`
	Rule   *RuleRef `json:"rule,omitempty"`
	Source ScopeID  `json:"source_scope"`
	Keeper *ScopeID `json:"keeper_scope,omitempty"`
	Deep   bool     `json:"deep"`
}

// PlanHeader is the immutable head of a plan. Its digest excludes item state.
type PlanHeader struct {
	ID           PlanID         `json:"plan_id"`
	Kind         string         `json:"kind"`
	CreatedAtNS  int64          `json:"created_at_ns"`
	ExpiresAtNS  int64          `json:"expires_at_ns"`
	Bindings     []ScopeBinding `json:"bindings"`
	PolicyDigest Digest         `json:"policy_digest"`
	ConfigDigest Digest         `json:"config_digest"`
	Digest       Digest         `json:"digest"`
}

// Approval carries the digest the user actually saw.
type Approval struct {
	Digest         Digest `json:"digest"`
	Source         string `json:"source"`
	WritersStopped bool   `json:"writers_stopped"`
}

// OperationResult is the per-item outcome of an apply.
//
// The three axes are separated because they answer different questions and the
// runner must not read one as another:
//
//	Code      what happened to this object (business result)
//	Control   what the caller should do next (continue / stop scope / stop run)
//	Evidence  whether an action was observed and persisted
//
// "Code is empty and error is nil" used to be the only signal a batch had, which
// is why a held scope could still be given a second rule's plan.
type OperationResult struct {
	Operation OperationID `json:"operation_id"`
	Item      ItemID      `json:"item_id"`
	State     string      `json:"state"`
	Code      string      `json:"code"`
	Message   string      `json:"message,omitempty"`
	Control   string      `json:"control,omitempty"`
	Evidence  string      `json:"evidence,omitempty"`
}

// Control results (section 6.2).
const (
	ControlContinue  = "continue"
	ControlStopPlan  = "stop_plan"
	ControlStopScope = "stop_scope"
	ControlStopRun   = "stop_run"
)

// Evidence results (section 6.2).
const (
	EvidenceNotStarted  = "not_started"
	EvidencePersisted   = "observed_and_persisted"
	EvidencePendingRecn = "pending_reconciliation"
)

// Operation result sources (section 11.1).
const (
	SourceObserved    = "observed"
	SourceReconciled  = "reconciled"
	SourceCompensated = "compensated"
)

// NewID generates a random 128-bit identifier with a short prefix. Object names
// in the vault are generated, never derived from user file names.
func NewID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("dustkeeper: entropy unavailable: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

// ObjectName is the vault file name for an item. It is exactly an object ID.
func ObjectName(item ItemID) string { return string(item) }

// DigestOf hashes a purpose tag plus canonical bytes. The tag prevents a digest
// computed for one purpose from being accepted for another.
func DigestOf(purpose string, canonical []byte) Digest {
	h := sha256.New()
	h.Write([]byte(purpose))
	h.Write([]byte("\n"))
	h.Write(canonical)
	return Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// Valid reports whether the digest has the project's canonical shape.
func (d Digest) Valid() bool {
	s := string(d)
	if len(s) != 7+64 || s[:7] != "sha256:" {
		return false
	}
	for _, c := range s[7:] {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// UniqueSorted copies, sorts and de-duplicates byte strings, which is how
// set-like fields are canonicalised before hashing.
func UniqueSorted(in [][]byte) [][]byte {
	if len(in) == 0 {
		return nil
	}
	cp := make([][]byte, len(in))
	for i, v := range in {
		c := make([]byte, len(v))
		copy(c, v)
		cp[i] = c
	}
	sort.Slice(cp, func(i, j int) bool { return bytes.Compare(cp[i], cp[j]) < 0 })
	out := cp[:0]
	for i, v := range cp {
		if i > 0 && bytes.Equal(out[len(out)-1], v) {
			continue
		}
		out = append(out, v)
	}
	return out
}
