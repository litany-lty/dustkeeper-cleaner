// Package assess produces read-only triage, never deletion authority.
package assess

import (
	"dustkeeper/internal/policy"
	"path/filepath"
	"time"
)

type Facts struct {
	Path                 string    `json:"path"`
	LogicalBytes         int64     `json:"logical_bytes"`
	AllocatedBytes       int64     `json:"allocated_bytes"`
	Device               uint64    `json:"device"`
	Inode                uint64    `json:"inode"`
	NLink                uint64    `json:"nlink"`
	CTimeNS              int64     `json:"ctime_ns"`
	Mode                 uint32    `json:"mode"`
	UID                  uint32    `json:"uid"`
	GID                  uint32    `json:"gid"`
	Regular              bool      `json:"regular"`
	MTime                time.Time `json:"mtime"`
	ATime                time.Time `json:"atime"`
	LastUsed             time.Time `json:"last_used,omitzero"`
	LastUsedSource       string    `json:"last_used_source,omitempty"`
	OpenByProcess        bool      `json:"open_by_visible_process,omitempty"`
	OpenDirectoryContext bool      `json:"open_directory_context,omitempty"`
	ProcessVisibility    string    `json:"process_visibility"`
	LastUsedStatus       string    `json:"last_used_status"`
	KnownReference       bool      `json:"known_reference"`
	TemporaryOrigin      string    `json:"temporary_origin,omitempty"`
	Protected            bool      `json:"protected"`
	OwnerConfirmedUnused bool      `json:"owner_confirmed_unused,omitempty"`
}
type Result struct {
	Path                   string   `json:"path"`
	Disposition            string   `json:"disposition"`
	Category               string   `json:"category"`
	Usage                  string   `json:"usage"`
	Reasons                []string `json:"reasons"`
	Missing                []string `json:"missing"`
	CanDelete              bool     `json:"can_delete"`
	LogicalBytes           int64    `json:"logical_bytes"`
	AllocatedBytes         int64    `json:"allocated_bytes"`
	QuarantineReclaimBytes int64    `json:"quarantine_reclaim_bytes"`
	PurgeReclaimUpperBound int64    `json:"purge_reclaim_upper_bound"`
}

func Assess(f Facts, days int, now time.Time) Result {
	return assess(f, days, now, false)
}

func AssessAdvisory(f Facts, days int, now time.Time) Result {
	return assess(f, days, now, true)
}

func assess(f Facts, days int, now time.Time, advisory bool) Result {
	r := Result{Path: f.Path, Disposition: "needs_review", Category: "unclassified", Usage: "unknown", Reasons: []string{}, Missing: []string{"disposal_intent", "recoverability", "reference_coverage"}, LogicalBytes: f.LogicalBytes, AllocatedBytes: f.AllocatedBytes}
	if f.OwnerConfirmedUnused {
		r.Missing = []string{"recoverability", "reference_coverage"}
	}
	protected, protectionReason := policy.GlobalProtected(filepath.ToSlash(f.Path))
	softProtection := policy.AdvisoryOnlyReviewable(filepath.ToSlash(f.Path))
	if protected && !softProtection {
		r.Disposition = "keep"
		r.Reasons = append(r.Reasons, "global_protection: "+protectionReason)
		return r
	}
	for _, check := range []struct {
		hit bool
		why string
	}{{!f.Regular, "not_regular"}, {f.NLink != 1, "link_count_not_one"}, {f.Protected, "explicit_protection"}, {f.OpenByProcess, "open_by_visible_process"}, {f.KnownReference, "known_reference"}} {
		if check.hit {
			r.Disposition = "keep"
			r.Reasons = append(r.Reasons, check.why)
			return r
		}
	}
	if days < 1 || days > 3650 {
		r.Disposition = "keep"
		r.Reasons = append(r.Reasons, "invalid_observation_window")
		return r
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	for _, v := range []time.Time{f.ATime, f.LastUsed, f.MTime} {
		if !v.IsZero() && !v.Before(cutoff) {
			r.Disposition = "keep"
			r.Usage = "recent_activity_observed"
			r.Reasons = append(r.Reasons, "recent_activity_or_change")
			return r
		}
	}
	// Names and document extensions indicate questions to ask, not permanent
	// exemptions. The table lives in ClassifyName so that the cleanup policy and
	// this report cannot drift into two different taxonomies.
	class := ClassifyName(filepath.Base(f.Path))
	r.Category = class.Category
	switch {
	case class.KeepName:
		r.Disposition = "keep"
		r.Reasons = append(r.Reasons, class.Reason)
		return r
	case class.Category == CatDocument:
		if class.ImportantName {
			r.Reasons = append(r.Reasons, "important_name_requires_purpose_review")
		}
		r.Reasons = append(r.Reasons, class.Reason)
		if !f.OwnerConfirmedUnused {
			r.Missing = append(r.Missing, "document_disposal_basis")
		}
	case class.Category == CatInstaller:
		r.Missing = append(r.Missing, "verified_retrieval_source")
	case class.Category == CatTemporary:
		if f.TemporaryOrigin != "" {
			r.Category = CatDeclaredTemp
			r.Reasons = append(r.Reasons, "origin_is_declared_not_deletion_authority")
		} else {
			r.Missing = append(r.Missing, "temporary_origin")
		}
	}
	if f.AllocatedBytes > 0 {
		r.PurgeReclaimUpperBound = f.AllocatedBytes
	}
	r.Reasons = append(r.Reasons, "old_or_missing_timestamps_do_not_prove_nonuse", "allocated_bytes_are_only_an_upper_bound")
	if protected && !advisory {
		r.Disposition, r.PurgeReclaimUpperBound = "keep", 0
		r.Reasons = append(r.Reasons, "global_protection: "+protectionReason)
	}
	return r
}

func archiveExtension(ext string) bool {
	switch ext {
	case ".zip", ".tar", ".gz", ".7z", ".tgz", ".tbz2", ".xz", ".rar", ".zst":
		return true
	default:
		return false
	}
}

func installerExtension(ext string) bool {
	switch ext {
	case ".dmg", ".pkg", ".msi", ".exe", ".appimage", ".deb", ".rpm", ".apk":
		return true
	default:
		return false
	}
}
