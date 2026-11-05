// Package cleanup turns a reviewed candidate into one of exactly three
// outcomes: permanently deleted, moved to the observation area, or left where
// it is.
//
// The package is deliberately pure: it decides, it does not act. Execution,
// identity checks and accounting stay in internal/execute, so a change here can
// never move a file on its own.
package cleanup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
	"dustkeeper/internal/strictjson"
)

// PolicyKey is the meta row that holds the one-time confirmation.
const PolicyKey = "cleanup.policy.v1"

// Notification channels. Only one exists on purpose: this build runs on a Mac
// and the document asks for one system notification plus a local details entry,
// not a notification platform.
const (
	NotifyMacOS = "macos_notification"
	NotifyNone  = "none"
)

// Observation day choices. 14 is the product default; 30 is what the six
// already-isolated objects were promised, so a user may pick it once.
const (
	DefaultObservationDays = 14
	MaxObservationDays     = 30
	MinObservationDays     = 7
)

// Policy is what the user confirms once: which directories are managed, which
// categories may be deleted outright, how long everything else is observed, how
// much one run may touch, and where the notice goes.
//
// It carries no per-file fields. Anything that has to be decided per file is
// derived from measured facts at the moment of the action.
type Policy struct {
	Version                int      `json:"version"`
	Scopes                 []string `json:"scopes"`
	DirectDeleteCategories []string `json:"direct_delete_categories"`
	ObservationDays        int      `json:"observation_days"`
	MaxFilesPerRun         int      `json:"max_files_per_run"`
	MaxBytesPerRun         int64    `json:"max_bytes_per_run"`
	Notify                 string   `json:"notify"`
	ResultDir              string   `json:"result_dir"`
	// KeepRetryDays is how long a keep decision suppresses asking the same
	// path again. Without it every run would pay to reject the same files.
	KeepRetryDays int `json:"keep_retry_days"`
	// ConfirmedAtNS and Digest record that a human approved this exact content.
	ConfirmedAtNS int64  `json:"confirmed_at_ns"`
	Digest        string `json:"digest,omitempty"`
}

// Default returns the policy before any confirmation: nothing may be deleted
// outright, everything movable is observed for 14 days.
func Default() Policy {
	return Policy{
		Version:                1,
		Scopes:                 []string{},
		DirectDeleteCategories: []string{},
		ObservationDays:        DefaultObservationDays,
		MaxFilesPerRun:         50,
		MaxBytesPerRun:         512 << 20,
		Notify:                 NotifyMacOS,
		KeepRetryDays:          14,
	}
}

// digest covers every field that changes what the program may do. The
// confirmation timestamps are excluded so that re-saving the same policy does
// not invalidate the digest the user saw.
func (p Policy) digestInput() Policy {
	c := p
	c.ConfirmedAtNS = 0
	c.Digest = ""
	c.Scopes = append([]string(nil), p.Scopes...)
	sort.Strings(c.Scopes)
	c.DirectDeleteCategories = append([]string(nil), p.DirectDeleteCategories...)
	sort.Strings(c.DirectDeleteCategories)
	return c
}

// ComputeDigest returns the digest a two-step confirmation shows and checks.
func (p Policy) ComputeDigest() (domain.Digest, error) {
	b, err := json.Marshal(p.digestInput())
	if err != nil {
		return "", errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return domain.DigestOf("dustkeeper/cleanup-policy/v1", b), nil
}

// Validate refuses anything this build would not know how to honour. Nothing is
// clamped: a policy that silently became something else is worse than a refusal.
func (p Policy) Validate() error {
	if p.Version != 1 {
		return errs.Newf(errs.CodeInvalidArgument, "cleanup policy version must be 1, got %d", p.Version)
	}
	if p.ObservationDays < MinObservationDays || p.ObservationDays > MaxObservationDays {
		return errs.Newf(errs.CodeInvalidArgument,
			"observation_days must be %d..%d, got %d", MinObservationDays, MaxObservationDays, p.ObservationDays)
	}
	if p.MaxFilesPerRun < 0 || p.MaxFilesPerRun > 10000 {
		return errs.Newf(errs.CodeInvalidArgument, "max_files_per_run must be 0..10000, got %d", p.MaxFilesPerRun)
	}
	if p.MaxBytesPerRun < 0 || p.MaxBytesPerRun > 1<<40 {
		return errs.Newf(errs.CodeInvalidArgument, "max_bytes_per_run must be 0..1TiB")
	}
	if p.KeepRetryDays < 0 || p.KeepRetryDays > 365 {
		return errs.Newf(errs.CodeInvalidArgument, "keep_retry_days must be 0..365, got %d", p.KeepRetryDays)
	}
	switch p.Notify {
	case NotifyMacOS, NotifyNone:
	default:
		return errs.Newf(errs.CodeInvalidArgument,
			"notify must be %q or %q, got %q", NotifyMacOS, NotifyNone, p.Notify)
	}
	if p.ResultDir != "" && (!filepath.IsAbs(p.ResultDir) || filepath.Clean(p.ResultDir) != p.ResultDir) {
		return errs.New(errs.CodeInvalidArgument, "result_dir must be an absolute normalized path")
	}
	for _, id := range p.Scopes {
		if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "/\\\x00") {
			return errs.Newf(errs.CodeInvalidArgument, "scope id %q is not a scope identifier", id)
		}
	}
	known := map[string]bool{}
	for _, c := range assess.KnownCategories() {
		known[c] = true
	}
	for _, c := range p.DirectDeleteCategories {
		if !known[c] {
			return errs.Newf(errs.CodeInvalidArgument, "unknown direct-delete category %q", c)
		}
		// The two protective classes can never be authorised for direct
		// deletion: accepting them would let a policy override the very
		// categories the classifier exists to keep.
		if c == assess.CatCredential || c == assess.CatUserRecord {
			return errs.Newf(errs.CodeInvalidArgument,
				"category %q requires retention and cannot be a direct-delete category", c)
		}
	}
	return nil
}

// DirectDelete reports whether a category may be deleted outright.
func (p Policy) DirectDelete(category string) bool {
	for _, c := range p.DirectDeleteCategories {
		if c == category {
			return true
		}
	}
	return false
}

// Covers reports whether a scope was included in the confirmation. An empty
// list after a confirmation would mean "no directory", never "all of them".
func (p Policy) Covers(scopeID string) bool {
	for _, s := range p.Scopes {
		if s == scopeID {
			return true
		}
	}
	return false
}

// RetentionSeconds is how long a newly isolated object is observed. Existing
// objects are never rewritten: their bound retention is read from their own row.
func (p Policy) RetentionSeconds() int64 {
	return int64(p.ObservationDays) * 24 * 60 * 60
}

// Load reads the confirmed policy. A missing row is the unconfirmed default, and
// the caller can tell the difference from Covers()/DirectDelete(): both answer
// "no" for the default.
func Load(ctx context.Context, st *store.Store) (Policy, error) {
	raw, err := st.MetaGet(ctx, PolicyKey)
	if err != nil {
		return Policy{}, err
	}
	if strings.TrimSpace(raw) == "" {
		return Default(), nil
	}
	opts := strictjson.Defaults()
	opts.StrictStruct = true
	var p Policy
	if err := strictjson.Unmarshal([]byte(raw), &p, opts); err != nil {
		return Policy{}, errs.Newf(errs.CodeStateWriteFailed,
			"the confirmed cleanup policy is unreadable; confirm it again with `cleanup setup`: %v", err)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Save writes a validated policy and records its digest.
func Save(ctx context.Context, st *store.Store, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	d, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	p.Digest = string(d)
	b, err := json.Marshal(p)
	if err != nil {
		return errs.Wrap(errs.CodeStateWriteFailed, err)
	}
	return st.MetaSet(ctx, PolicyKey, string(b))
}

// Confirmed reports whether a human approved this policy.
func (p Policy) Confirmed() bool { return p.ConfirmedAtNS > 0 }
