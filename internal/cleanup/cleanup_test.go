package cleanup

import (
	"strings"
	"testing"

	"dustkeeper/internal/domain"
)

func TestDefaultPolicyDeletesNothing(t *testing.T) {
	p := Default()
	if err := p.Validate(); err != nil {
		t.Fatalf("default policy must be valid: %v", err)
	}
	for _, c := range []string{"installer_candidate", "temporary_name_only", "stale_or_unknown"} {
		if p.DirectDelete(c) {
			t.Fatalf("default policy allows direct deletion of %s", c)
		}
	}
	if p.Covers("downloads") {
		t.Fatal("the unconfirmed policy must not cover any scope")
	}
	if p.RetentionSeconds() != 14*24*60*60 {
		t.Fatalf("default observation is %d seconds", p.RetentionSeconds())
	}
}

func TestPolicyRejectsRetentionCategoriesAndTypos(t *testing.T) {
	p := Default()
	p.DirectDeleteCategories = []string{"potential_credential_or_recovery_data"}
	if err := p.Validate(); err == nil {
		t.Fatal("credentials must never be a direct-delete category")
	}
	p.DirectDeleteCategories = []string{"potential_user_record"}
	if err := p.Validate(); err == nil {
		t.Fatal("user records must never be a direct-delete category")
	}
	p.DirectDeleteCategories = []string{"installer_candidates"}
	if err := p.Validate(); err == nil {
		t.Fatal("an unknown category must be refused rather than match nothing")
	}
	p = Default()
	p.ObservationDays = 0
	if err := p.Validate(); err == nil {
		t.Fatal("zero observation days accepted")
	}
	p = Default()
	p.Notify = "telegram"
	if err := p.Validate(); err == nil {
		t.Fatal("an unsupported notice channel accepted")
	}
}

func TestPolicyDigestIgnoresConfirmationTimeButNotContent(t *testing.T) {
	a := Default()
	da, err := a.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	b := a
	b.ConfirmedAtNS = 12345
	db, err := b.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatal("re-confirming the same policy changed its digest")
	}
	c := a
	c.DirectDeleteCategories = []string{"installer_candidate"}
	dc, err := c.ComputeDigest()
	if err != nil {
		t.Fatal(err)
	}
	if da == dc {
		t.Fatal("adding a direct-delete category did not change the digest")
	}
}

func TestDecideProducesOnlyThreeOutcomes(t *testing.T) {
	base := Inputs{
		Action:        domain.DispositionDelete,
		Verdict:       domain.VerdictAllow,
		FactsComplete: true,
		Movable:       true,
		Authorized:    true,
	}
	if d, r := Decide(base); d != domain.DispositionDelete || r != ReasonDualReviewAgrees {
		t.Fatalf("both reviewers allowed and authority is present: got %s/%s", d, r)
	}

	cases := []struct {
		name string
		in   Inputs
		want string
		why  string
	}{
		{"model service failed", Inputs{Action: domain.DispositionDelete, ModelFailed: true, Movable: true, Authorized: true, Verdict: domain.VerdictAllow}, domain.DispositionKeep, ReasonReviewUnavailable},
		{"facts missing", Inputs{Action: domain.DispositionDelete, Verdict: domain.VerdictAllow, Movable: true, Authorized: true}, domain.DispositionKeep, ReasonFactsMissing},
		{"explicit keep", Inputs{Action: domain.DispositionDelete, Verdict: domain.VerdictKeep, FactsComplete: true, Movable: true, Authorized: true}, domain.DispositionKeep, ReasonModelSaysKeep},
		{"cannot move safely", Inputs{Action: domain.DispositionDelete, Verdict: domain.VerdictAllow, FactsComplete: true, Authorized: true}, domain.DispositionKeep, ReasonMoveSafetyUnknown},
		{"no permanent-delete authority", Inputs{Action: domain.DispositionDelete, Verdict: domain.VerdictAllow, FactsComplete: true, Movable: true}, domain.DispositionKeep, ReasonNoAuthority},
		{"insufficient for deletion", Inputs{Action: domain.DispositionDelete, Verdict: domain.VerdictInsufficient, FactsComplete: true, Movable: true, Authorized: true}, domain.DispositionQuarantine, ReasonNotEnoughForDeletion},
		{"isolation allowed", Inputs{Action: domain.DispositionQuarantine, Verdict: domain.VerdictAllow, FactsComplete: true, Movable: true}, domain.DispositionQuarantine, ReasonSafeToObserve},
		{"isolation unproven", Inputs{Action: domain.DispositionQuarantine, Verdict: domain.VerdictInsufficient, FactsComplete: true, Movable: true}, domain.DispositionKeep, ReasonMoveSafetyUnknown},
		{"isolation refused", Inputs{Action: domain.DispositionQuarantine, Verdict: domain.VerdictKeep, FactsComplete: true, Movable: true}, domain.DispositionKeep, ReasonModelSaysKeep},
	}
	for _, tc := range cases {
		d, r := Decide(tc.in)
		if d != tc.want || r != tc.why {
			t.Fatalf("%s: got %s/%s want %s/%s", tc.name, d, r, tc.want, tc.why)
		}
	}
}

// A single explicit keep must survive even when the other reviewer allowed the
// action: the normalisation that produces VerdictKeep is what enforces it, and
// this test pins the rule that a decision can never read as delete afterwards.
func TestKeepCannotBeOverriddenByAuthority(t *testing.T) {
	d, _ := Decide(Inputs{
		Action:        domain.DispositionDelete,
		Verdict:       domain.VerdictKeep,
		FactsComplete: true,
		Movable:       true,
		Authorized:    true,
	})
	if d != domain.DispositionKeep {
		t.Fatalf("a keep verdict produced %s", d)
	}
}

func TestEveryReasonHasReadableText(t *testing.T) {
	for _, r := range []string{ReasonReviewUnavailable, ReasonFactsMissing, ReasonModelSaysKeep,
		ReasonMoveSafetyUnknown, ReasonNoAuthority, ReasonNotEnoughForDeletion,
		ReasonSafeToObserve, ReasonDualReviewAgrees} {
		if txt := ReasonText(r); txt == r || strings.TrimSpace(txt) == "" {
			t.Fatalf("reason %q has no readable text", r)
		}
	}
}
