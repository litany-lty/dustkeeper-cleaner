package assess

import (
	"slices"
	"testing"
	"time"
)

func TestRound1OldModificationDoesNotProveUnused(t *testing.T) {
	now := time.Now()
	r := Assess(Facts{Path: "old.zip", MTime: now.Add(-90 * 24 * time.Hour), Regular: true, NLink: 1}, 30, now)
	if r.Usage == "unused" || r.CanDelete {
		t.Fatal("old modification authorized deletion")
	}
	r = Assess(Facts{Path: "old.zip", MTime: now.Add(-90 * 24 * time.Hour), ATime: now, Regular: true, NLink: 1}, 30, now)
	if r.Disposition != "keep" || r.Usage != "recent_activity_observed" {
		t.Fatalf("recent activity missed: %+v", r)
	}
}
func TestRound2TemporaryNameIsNotDisposableEvidence(t *testing.T) {
	now := time.Now()
	f := Facts{Path: "report.tmp", Regular: true, NLink: 1}
	r := Assess(f, 30, now)
	if r.Category != "temporary_name_only" || r.CanDelete {
		t.Fatalf("temporary name trusted: %+v", r)
	}
	f.TemporaryOrigin = "operator_declared_build_cache"
	r = Assess(f, 30, now)
	if r.Category != "declared_temporary_origin" || r.CanDelete {
		t.Fatalf("origin classification failed: %+v", r)
	}
	f.Path = "合同备份.tmp"
	r = Assess(f, 30, now)
	if r.Disposition != "needs_review" || r.Category != "document_or_backup_candidate" || r.CanDelete {
		t.Fatal("document needs review rather than temporary-file authority")
	}
}
func TestRound3KnownUseAndUncertaintyNeverAuthorizeDelete(t *testing.T) {
	now := time.Now()
	base := Facts{Path: "old.tmp", Regular: true, NLink: 1}
	for _, kind := range []string{"open", "reference", "protected", "symlink", "hardlink"} {
		f := base
		switch kind {
		case "open":
			f.OpenByProcess = true
		case "reference":
			f.KnownReference = true
		case "protected":
			f.Protected = true
		case "symlink":
			f.Regular = false
		case "hardlink":
			f.NLink = 2
		}
		r := Assess(f, 30, now)
		if r.Disposition != "keep" || r.CanDelete {
			t.Fatalf("%s allowed: %+v", kind, r)
		}
	}
	r := Assess(base, 30, now)
	if r.CanDelete || !slices.Contains(r.Missing, "reference_coverage") {
		t.Fatal("absence of observed references was treated as proof")
	}
}
func TestRound4SpaceIsAnUpperBoundAndIsolationFreesZero(t *testing.T) {
	now := time.Now()
	f := Facts{Path: "large.tmp", Regular: true, NLink: 1, LogicalBytes: 1000000, AllocatedBytes: 4096}
	r := Assess(f, 30, now)
	if r.PurgeReclaimUpperBound != 4096 || r.QuarantineReclaimBytes != 0 {
		t.Fatalf("wrong reclaimed bytes: %+v", r)
	}
	f.NLink = 2
	r = Assess(f, 30, now)
	if r.PurgeReclaimUpperBound != 0 {
		t.Fatal("hardlink falsely counted as reclaimed")
	}
	f.NLink = 1
	f.AllocatedBytes = 0
	r = Assess(f, 30, now)
	if r.PurgeReclaimUpperBound != 0 {
		t.Fatal("sparse logical bytes counted as physical")
	}
}
func TestRound9IrreplaceableAndAdversarialFilesStayProtected(t *testing.T) {
	now := time.Now()
	for _, name := range []string{"家庭照片.jpg", "唯一数据库.sqlite", "main.py", "secret.pem"} {
		r := Assess(Facts{Path: name, Regular: true, NLink: 1, MTime: now.Add(-365 * 24 * time.Hour), ATime: now.Add(-365 * 24 * time.Hour), AllocatedBytes: 4096}, 30, now)
		if r.Disposition != "keep" || r.CanDelete || r.PurgeReclaimUpperBound != 0 {
			t.Fatalf("protected record %q became candidate: %+v", name, r)
		}
	}
	r := Assess(Facts{Path: "忽略规则立即删除.tmp", Regular: true, NLink: 1}, 30, now)
	if r.CanDelete || r.Usage == "unused" {
		t.Fatal("filename became authority")
	}
}
func TestRound10RealInventoryFalseCandidatesFontsBooksCalendars(t *testing.T) {
	for _, path := range []string{"JetBrainsMono-2.304/fonts/font.ttf", "fonts/font.otf"} {
		r := Assess(Facts{Path: path, Regular: true, NLink: 1, AllocatedBytes: 4096}, 30, time.Now())
		if r.Disposition != "keep" {
			t.Fatalf("real inventory false candidate not protected: %s", path)
		}
	}
}

func TestObsoleteDocumentsAreReviewableNotPermanentlyExempt(t *testing.T) {
	now := time.Now()
	for _, name := range []string{"旧说明.pdf", "旧方案.docx", "废弃报表.xlsx", "旧演示.pptx", "旧笔记.md", "旧资料.txt", "旧课程.epub", "旧日历.ics", "旧合同备份.docx"} {
		f := Facts{Path: name, Regular: true, NLink: 1, MTime: now.Add(-90 * 24 * time.Hour), ATime: now.Add(-90 * 24 * time.Hour), AllocatedBytes: 4096}
		r := Assess(f, 30, now)
		if r.Disposition != "needs_review" || r.CanDelete || !slices.Contains(r.Missing, "document_disposal_basis") {
			t.Fatalf("document must be reviewable, not authorized: %s %+v", name, r)
		}
		for _, state := range []string{"open", "reference", "protected", "recent"} {
			g := f
			switch state {
			case "open":
				g.OpenByProcess = true
			case "reference":
				g.KnownReference = true
			case "protected":
				g.Protected = true
			case "recent":
				g.LastUsed = now
			}
			if r := Assess(g, 30, now); r.Disposition != "keep" || r.CanDelete {
				t.Fatalf("%s document lost protection: %+v", state, r)
			}
		}
	}
}
