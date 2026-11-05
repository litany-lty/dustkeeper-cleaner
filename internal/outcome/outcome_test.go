package outcome

import (
	"testing"

	"dustkeeper/internal/domain"
)

// allowed is one row of the product's contract: this input produces this
// disposition for this reason.
type allowed struct {
	input  Inputs
	want   string
	reason string
}

func TestEveryKeepReasonIsItsOwnCase(t *testing.T) {
	// R04 names four different things that all end in "left where it is", and
	// each one is a different sentence to the user. Collapsing them would make
	// "the model service is down" indistinguishable from "the models say this
	// file is still needed", which is precisely the confusion the product was
	// rewritten to remove.
	allow := func() Inputs {
		return Inputs{
			Action:        domain.DispositionDelete,
			Verdict:       domain.VerdictAllow,
			FactsComplete: true,
			Movable:       true,
			Authorized:    true,
		}
	}
	cases := []struct {
		name   string
		mutate func(*Inputs)
		reason string
	}{
		{"model service failed", func(in *Inputs) { in.ModelFailed = true }, ReasonReviewUnavailable},
		{"a needed fact was not measured", func(in *Inputs) { in.FactsComplete = false }, ReasonFactsMissing},
		{"a reviewer said keep", func(in *Inputs) { in.Verdict = domain.VerdictKeep }, ReasonModelSaysKeep},
		{"not provably movable", func(in *Inputs) { in.Movable = false }, ReasonMoveSafetyUnknown},
		{"no standing authority", func(in *Inputs) { in.Authorized = false }, ReasonNoAuthority},
	}
	seen := map[string]struct{}{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := allow()
			tc.mutate(&in)
			disposition, reason := Decide(in)
			if disposition != domain.DispositionKeep {
				t.Fatalf("disposition=%q, want %q", disposition, domain.DispositionKeep)
			}
			if reason != tc.reason {
				t.Fatalf("reason=%q, want %q", reason, tc.reason)
			}
			seen[reason] = struct{}{}
		})
	}
	// Four of these are R04's list; the fifth, no_authority, is the boundary
	// case R05 introduces. Every reason must be separately reachable and
	// separately named.
	for _, reason := range []string{
		ReasonReviewUnavailable, ReasonFactsMissing, ReasonModelSaysKeep,
		ReasonMoveSafetyUnknown, ReasonNoAuthority,
	} {
		if _, ok := seen[reason]; !ok {
			t.Fatalf("reason %q has no case", reason)
		}
	}
}

// TestFailedReviewIsNeverAnApproval is the first stop condition: deletion must
// be impossible when the reviewers never answered.
func TestFailedReviewIsNeverAnApproval(t *testing.T) {
	for _, action := range []string{domain.DispositionDelete, domain.DispositionQuarantine, domain.ActionPurge} {
		for _, verdict := range []string{domain.VerdictAllow, domain.VerdictInsufficient, domain.VerdictKeep} {
			disposition, reason := Decide(Inputs{
				Action: action, Verdict: verdict, ModelFailed: true,
				FactsComplete: true, Movable: true, Authorized: true,
			})
			if disposition != domain.DispositionKeep || reason != ReasonReviewUnavailable {
				t.Fatalf("action=%s verdict=%s failed review produced %s/%s", action, verdict, disposition, reason)
			}
		}
	}
}

// TestUnmeasuredFactIsNotFilledInByAModel pins R17: the program may not accept
// an allow for a decision whose inputs it did not measure.
func TestUnmeasuredFactIsNotFilledInByAModel(t *testing.T) {
	for _, action := range []string{domain.DispositionDelete, domain.DispositionQuarantine, domain.ActionPurge} {
		disposition, reason := Decide(Inputs{
			Action: action, Verdict: domain.VerdictAllow,
			FactsComplete: false, Movable: true, Authorized: true,
		})
		if disposition != domain.DispositionKeep || reason != ReasonFactsMissing {
			t.Fatalf("action=%s incomplete facts produced %s/%s", action, disposition, reason)
		}
	}
	// Missing facts outrank a stated keep: the user is told the stronger fact
	// (nothing was measured), not the weaker one.
	if _, reason := Decide(Inputs{
		Action: domain.DispositionDelete, Verdict: domain.VerdictKeep, FactsComplete: false, Movable: true,
	}); reason != ReasonFactsMissing {
		t.Fatalf("facts_missing should win over model_says_keep, got %q", reason)
	}
}

// TestExplicitKeepIsFinal pins R14: an allow from the other side never
// overrides a keep, whichever question was asked.
func TestExplicitKeepIsFinal(t *testing.T) {
	for _, action := range []string{domain.DispositionDelete, domain.DispositionQuarantine, domain.ActionPurge} {
		disposition, reason := Decide(Inputs{
			Action: action, Verdict: domain.VerdictKeep,
			FactsComplete: true, Movable: true, Authorized: true,
		})
		if disposition != domain.DispositionKeep || reason != ReasonModelSaysKeep {
			t.Fatalf("action=%s keep verdict produced %s/%s", action, disposition, reason)
		}
	}
}

// TestMoveSafetyIsDecidedLocally pins R05. The two boundaries are deliberately
// different: "not sure it should be deleted" may be observed, "not sure it can
// even be moved" may not.
func TestMoveSafetyIsDecidedLocally(t *testing.T) {
	// Unsure about deletion, but moving is provably safe: observation.
	disposition, reason := Decide(Inputs{
		Action: domain.DispositionDelete, Verdict: domain.VerdictInsufficient,
		FactsComplete: true, Movable: true, Authorized: true,
	})
	if disposition != domain.DispositionQuarantine || reason != ReasonNotEnoughForDeletion {
		t.Fatalf("insufficient on delete gave %s/%s", disposition, reason)
	}
	// Unsure about movement itself: stay. Note the answer is the same when the
	// reviewers said allow, because movability is a local fact.
	for _, verdict := range []string{domain.VerdictAllow, domain.VerdictInsufficient} {
		disposition, reason := Decide(Inputs{
			Action: domain.DispositionQuarantine, Verdict: verdict,
			FactsComplete: true, Movable: false, Authorized: true,
		})
		if disposition != domain.DispositionKeep || reason != ReasonMoveSafetyUnknown {
			t.Fatalf("verdict=%s unmovable produced %s/%s", verdict, disposition, reason)
		}
	}
	// Insufficient on the quarantine question means movement is unproven, and
	// that is a keep even when the object is movable.
	if disposition, reason := Decide(Inputs{
		Action: domain.DispositionQuarantine, Verdict: domain.VerdictInsufficient,
		FactsComplete: true, Movable: true, Authorized: true,
	}); disposition != domain.DispositionKeep || reason != ReasonMoveSafetyUnknown {
		t.Fatalf("insufficient on quarantine gave %s/%s", disposition, reason)
	}
}

// TestDeleteNeedsBothAgreementAndAuthority pins R02: three things, none of which
// substitutes for another.
func TestDeleteNeedsBothAgreementAndAuthority(t *testing.T) {
	base := Inputs{
		Action: domain.DispositionDelete, Verdict: domain.VerdictAllow,
		FactsComplete: true, Movable: true, Authorized: true,
	}
	if disposition, reason := Decide(base); disposition != domain.DispositionDelete || reason != ReasonDualReviewAgrees {
		t.Fatalf("full agreement gave %s/%s", disposition, reason)
	}
	// The same agreement with no grant is not a deletion; it is not even an
	// observation, because the category was never approved for observation
	// either - this build simply has no standing to act.
	noGrant := base
	noGrant.Authorized = false
	if disposition, reason := Decide(noGrant); disposition != domain.DispositionKeep || reason != ReasonNoAuthority {
		t.Fatalf("agreement without authority gave %s/%s", disposition, reason)
	}
	// A quarantine answer cannot become a deletion by changing the question.
	quarantineAllow := Inputs{
		Action: domain.DispositionQuarantine, Verdict: domain.VerdictAllow,
		FactsComplete: true, Movable: true, Authorized: true,
	}
	if disposition, _ := Decide(quarantineAllow); disposition != domain.DispositionQuarantine {
		t.Fatalf("quarantine allow produced %q", disposition)
	}
}

// TestPurgeSharesTheDeleteRule pins that expiry clearing faces the same bar as a
// first-time deletion: time spent in the observation area is an extra check, not
// a substitute for authority.
func TestPurgeSharesTheDeleteRule(t *testing.T) {
	if disposition, reason := Decide(Inputs{
		Action: domain.ActionPurge, Verdict: domain.VerdictAllow,
		FactsComplete: true, Movable: true, Authorized: true,
	}); disposition != domain.DispositionDelete || reason != ReasonDualReviewAgrees {
		t.Fatalf("purge with agreement gave %s/%s", disposition, reason)
	}
	if disposition, reason := Decide(Inputs{
		Action: domain.ActionPurge, Verdict: domain.VerdictAllow,
		FactsComplete: true, Movable: true, Authorized: false,
	}); disposition != domain.DispositionKeep || reason != ReasonNoAuthority {
		t.Fatalf("purge without authority gave %s/%s", disposition, reason)
	}
	if disposition, reason := Decide(Inputs{
		Action: domain.ActionPurge, Verdict: domain.VerdictInsufficient,
		FactsComplete: true, Movable: true, Authorized: true,
	}); disposition != domain.DispositionQuarantine || reason != ReasonNotEnoughForDeletion {
		t.Fatalf("purge with insufficient gave %s/%s", disposition, reason)
	}
}

// TestExhaustive: over every combination of the inputs, exactly the three
// documented dispositions appear, and deletion appears only when everything
// holds. This is the test that makes the "no accidental delete" claim something
// a reviewer can check rather than something the author asserts.
func TestExhaustive(t *testing.T) {
	actions := []string{domain.DispositionDelete, domain.DispositionQuarantine, domain.ActionPurge, "", "unexpected"}
	verdicts := []string{domain.VerdictAllow, domain.VerdictInsufficient, domain.VerdictKeep, "", "unexpected"}
	bools := []bool{true, false}
	seenDisposition := map[string]struct{}{}
	combinations := 0
	for _, action := range actions {
		for _, verdict := range verdicts {
			for _, failed := range bools {
				for _, complete := range bools {
					for _, movable := range bools {
						for _, authorized := range bools {
							combinations++
							in := Inputs{
								Action: action, Verdict: verdict, ModelFailed: failed,
								FactsComplete: complete, Movable: movable, Authorized: authorized,
							}
							disposition, reason := Decide(in)
							seenDisposition[disposition] = struct{}{}
							if reason == "" {
								t.Fatalf("%+v produced an empty reason", in)
							}
							if text := ReasonText(reason); text == "" || text == reason {
								t.Fatalf("%+v produced an untranslated reason %q", in, reason)
							}
							if disposition == domain.DispositionDelete {
								irreversible := action == domain.DispositionDelete || action == domain.ActionPurge
								if !irreversible || failed || !complete || !movable || !authorized || verdict != domain.VerdictAllow {
									t.Fatalf("%+v produced a deletion", in)
								}
							}
						}
					}
				}
			}
		}
	}
	if combinations != 5*5*2*2*2*2 {
		t.Fatalf("enumerated %d combinations", combinations)
	}
	for _, want := range []string{domain.DispositionDelete, domain.DispositionQuarantine, domain.DispositionKeep} {
		if _, ok := seenDisposition[want]; !ok {
			t.Fatalf("disposition %q is unreachable", want)
		}
	}
	if len(seenDisposition) != 3 {
		t.Fatalf("produced %d distinct dispositions: %v", len(seenDisposition), seenDisposition)
	}
}
