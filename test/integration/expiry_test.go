package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// The clearing step is the last irreversible thing the product does to a file it
// moved on the user's behalf. Before it spends any of a run's allowance, each due
// object passes a pre-flight, and every refusal has to be explainable.
func TestDueObjectThatCannotBeClearedIsSkippedWithAReason(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, e *env, sc store.Scope, it store.Item)
		want    string
	}{
		{
			// The user is working in that place again. Nothing about the vault
			// object changed, but the one piece of evidence the product cannot
			// measure any other way - somebody put something back - is here.
			name: "a file is back at the original path",
			prepare: func(t *testing.T, e *env, _ store.Scope, _ store.Item) {
				e.writeFile("exports", "a.zip", "the user put this here", 2*time.Hour)
			},
			want: "path_reused",
		},
		{
			name: "the original path was protected after the move",
			prepare: func(t *testing.T, e *env, sc store.Scope, _ store.Item) {
				e.insertProtection(sc, "a.zip", domain.MatchExact)
			},
			want: "protected",
		},
		{
			name: "the vault object is not the one that was moved",
			prepare: func(t *testing.T, e *env, _ store.Scope, it store.Item) {
				path := e.vaultPath("exports", it.ObjectName)
				if err := os.Remove(path); err != nil {
					t.Fatalf("remove the vault object: %v", err)
				}
				if err := os.WriteFile(path, []byte("not the object that was moved"), 0o600); err != nil {
					t.Fatalf("replace the vault object: %v", err)
				}
			},
			want: "object_changed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, sc, it := dueObjectAcrossSevenDays(t)
			tc.prepare(t, e, sc, it)

			// This step carries the clock past the window with the disturbance
			// already in place, so the refusal happens at the only moment it
			// matters.
			rep := e.step(47 * time.Hour)
			if rep.Purged != 0 {
				t.Fatalf("the run cleared an object the review refused: %+v", rep)
			}
			if len(rep.Scopes) != 1 || rep.Scopes[0].DueNotAuthorised != 1 {
				t.Fatalf("the refusal was not counted: %+v", rep.Scopes)
			}
			skipped := strings.Join(rep.Scopes[0].ExpirySkipped, "\n")
			if !strings.Contains(skipped, tc.want) {
				t.Fatalf("the run does not say why the object was left alone: %q", skipped)
			}
			if _, err := os.Stat(e.vaultPath("exports", it.ObjectName)); err != nil {
				t.Fatalf("the vault object was removed anyway: %v", err)
			}
		})
	}
}

// TestExpiryReviewRefusesWhatItCannotDescribe covers the two refusals that a run
// cannot produce by itself, because the queue never hands them over.
func TestExpiryReviewRefusesWhatItCannotDescribe(t *testing.T) {
	t.Run("an object that is no longer in the observation area", func(t *testing.T) {
		e, sc, res := scheduledDeleteSetup(t)
		sctx := e.scopeCtx(sc.ID)
		why, err := e.exec.ReviewExpiry(context.Background(), sctx, res.Items[0])
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		if !strings.HasPrefix(why, "not_in_observation_area") {
			t.Fatalf("reason = %q", why)
		}
	})

	t.Run("an object with no complete post-move evidence", func(t *testing.T) {
		e, sc, it, _, _ := safetyReviewDueObject(t)
		sctx := e.scopeCtx(sc.ID)
		// The evidence is what a permanent deletion has to be able to describe.
		// Without it the object is not clearable, whatever the calendar says.
		why, err := e.exec.ReviewExpiry(context.Background(), sctx, withMissingEvidence(it))
		if err != nil {
			t.Fatalf("review: %v", err)
		}
		if !strings.HasPrefix(why, "evidence_incomplete") {
			t.Fatalf("reason = %q", why)
		}
	})
}

// withMissingEvidence returns the item as the store would hold it if the
// post-move hash had never been written.
func withMissingEvidence(it store.Item) store.Item {
	it.MetadataHash = nil
	return it
}

// dueObjectAcrossSevenDays quarantines one object under a seven-day rule and
// walks the clock past its window in steps a scheduler would use, never crossing
// the large-jump threshold. The object is due and authorised when it returns.
func dueObjectAcrossSevenDays(t *testing.T) (*env, store.Scope, store.Item) {
	t.Helper()
	e := newEnv(t)
	e.configure(func(c *config.Config) { c.Runtime.MaxAutoPurgeFiles = 2 })
	sc := e.addScope("exports", domain.KindManagedDisposable)
	e.writeFile("exports", "a.zip", "payload-a", 40*24*time.Hour)
	e.scan(sc.ID)
	e.confirmPolicy(7, sc.ID)
	r := e.rule(sc.ID, "rule-1", []string{"*.zip"}, nil, 30*24*time.Hour, 7*24*time.Hour)
	e.grant(r, domain.CapAutoQuarantine, 10, 1<<30)
	e.grant(r, domain.CapAutoPurge, 10, 1<<30)

	if rep := e.runOnce(); rep.Quarantined != 1 {
		t.Fatalf("expected one object to be quarantined, got %+v", rep)
	}
	items := e.quarantinedItems(sc.ID)
	if len(items) != 1 {
		t.Fatalf("expected one object in the observation area, got %d", len(items))
	}
	// Walk towards the deadline in steps a scheduler would use. Three steps leave
	// the object just short of its window, so a caller can disturb something and
	// then take the one step that crosses it.
	for i := 0; i < 3; i++ {
		if rep := e.step(47 * time.Hour); rep.Purged != 0 {
			t.Fatalf("step %d cleared an object before its window elapsed: %+v", i, rep)
		}
	}
	return e, sc, items[0]
}

// TestDueObjectWithNothingChangedIsCleared is the control for the test above: the
// same fixture, with nothing disturbed, really does clear the object. Without it
// the refusals would prove nothing.
func TestDueObjectWithNothingChangedIsCleared(t *testing.T) {
	e, sc, it := dueObjectAcrossSevenDays(t)
	if rep := e.step(47 * time.Hour); rep.Purged != 1 {
		t.Fatalf("a due and authorised object was not cleared: %+v", rep)
	}
	if _, err := os.Stat(e.vaultPath("exports", it.ObjectName)); !os.IsNotExist(err) {
		t.Fatalf("the vault object is still there: %v", err)
	}
	if items := e.quarantinedItems(sc.ID); len(items) != 0 {
		t.Fatalf("%d object(s) remain in the observation area", len(items))
	}
}
