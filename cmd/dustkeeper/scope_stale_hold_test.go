package main

// C05, the half that cannot be fixed by committing two writes together: an
// installation that already went through the old window has a scope held over
// nothing. The object was compensated and settled; the scope was never released
// with it; there is no pending operation left for reconciliation to pick up.
//
// Releasing that automatically would be guessing, because a held scope has more
// than one cause (a root that no longer proves itself does it too, and the schema
// stores no reason). Refusing to release it at all is what made the state
// permanent: the only advice the command gave was "re-register", which sends the
// operator to change a registration that is fine.
//
// So the operator gets the fact and an explicit way to accept it. Nothing widens:
// while any object in the scope is unresolved the flag is refused, and the root
// is still checked every time the scope is opened.

import (
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/store"
)

// openState opens the state database of a fixture directly. It is how these
// tests recreate a state the current build cannot produce on purpose.
func openState(t *testing.T, base string) *store.Store {
	t.Helper()
	st, err := store.OpenExisting(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open the state database: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestAScopeHeldOverNothingCanBeResumedOnPurpose covers the historical state:
// the object is settled, the scope is held, nothing is pending.
func TestAScopeHeldOverNothingCanBeResumedOnPurpose(t *testing.T) {
	base, itemID := heldObjectFixture(t)
	st := openState(t, base)

	if err := st.MarkItemState(t.Context(), domain.ItemID(itemID), domain.StateRestored, ""); err != nil {
		t.Fatalf("settle the item: %v", err)
	}
	if err := st.SetScopeStatus(t.Context(), "exports", domain.ScopeHeld); err != nil {
		t.Fatalf("hold the scope: %v", err)
	}

	// Without the flag the command refuses, but it must say what the state is.
	// "Re-register" was the only advice before, and it is wrong for this state.
	r := cli(t, "scope", "resume", "--id", "exports", "--json")
	if r.code == 0 {
		t.Fatalf("a held scope was resumed without being asked about the hold:\n%s", r.stdout)
	}
	if got := r.str(t, "code"); got != string("HELD") {
		t.Fatalf("code = %q, want HELD; stdout:\n%s", got, r.stdout)
	}
	if !strings.Contains(r.stdout, "--accept-stale-hold") {
		t.Fatalf("the refusal does not name the way out:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "no") || !strings.Contains(r.stdout, "unresolved") {
		t.Fatalf("the refusal does not say why the hold no longer applies:\n%s", r.stdout)
	}
	if got, err := st.GetScope(t.Context(), "exports"); err != nil {
		t.Fatalf("get scope: %v", err)
	} else if got.Status != domain.ScopeHeld {
		t.Fatalf("a refused resume changed the scope to %s", got.Status)
	}

	// With it, the operator's decision is recorded, and only that.
	ok := mustCLI(t, "scope", "resume", "--id", "exports", "--accept-stale-hold", "--json")
	if got := dataString(t, ok, "status"); got != domain.ScopeActive {
		t.Fatalf("status = %q, want active", got)
	}
	if got := dataString(t, ok, "previous"); got != domain.ScopeHeld {
		t.Fatalf("previous = %q, want held", got)
	}
	// Accepting a stale hold is about the scope row, not about the object: the
	// item stays exactly where it was.
	item, err := st.GetItem(t.Context(), domain.ItemID(itemID))
	if err != nil {
		t.Fatalf("get item: %v", err)
	}
	if item.State != domain.StateRestored {
		t.Fatalf("the item is %s after the resume, want restored", item.State)
	}
}

// TestAStaleHoldCannotBeAcceptedWhileAnObjectIsUnresolved is the boundary: the
// flag is not a way around a hold that still has a subject.
func TestAStaleHoldCannotBeAcceptedWhileAnObjectIsUnresolved(t *testing.T) {
	base, _ := heldObjectFixture(t) // its object is held, so it is unresolved
	st := openState(t, base)
	if err := st.SetScopeStatus(t.Context(), "exports", domain.ScopeHeld); err != nil {
		t.Fatalf("hold the scope: %v", err)
	}

	r := cli(t, "scope", "resume", "--id", "exports", "--accept-stale-hold", "--json")
	if r.code == 0 {
		t.Fatalf("--accept-stale-hold resumed a scope whose object is still held:\n%s", r.stdout)
	}
	if got := r.str(t, "code"); got != "HELD" {
		t.Fatalf("code = %q, want HELD; stdout:\n%s", got, r.stdout)
	}
	if !strings.Contains(r.stdout, "unresolved object") {
		t.Fatalf("the refusal does not name the unresolved object:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "quarantine list") {
		t.Fatalf("the refusal does not say where to look:\n%s", r.stdout)
	}
	got, err := st.GetScope(t.Context(), "exports")
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if got.Status != domain.ScopeHeld {
		t.Fatalf("the scope is %s; the flag resumed it anyway", got.Status)
	}
}

// TestAnOrdinaryReapprovalDoesNotNeedTheStaleHoldFlag keeps the new flag from
// becoming a second way to do what resume already does. A scope that needs
// reapproval has a reason to be there, and resuming it never needed a flag.
func TestAnOrdinaryReapprovalDoesNotNeedTheStaleHoldFlag(t *testing.T) {
	base, _ := heldObjectFixture(t)
	st := openState(t, base)
	if err := st.SetScopeStatus(t.Context(), "exports", domain.ScopeNeedsReapproval); err != nil {
		t.Fatalf("mark needs_reapproval: %v", err)
	}

	ok := mustCLI(t, "scope", "resume", "--id", "exports", "--json")
	if got := dataString(t, ok, "previous"); got != domain.ScopeNeedsReapproval {
		t.Fatalf("previous = %q, want needs_reapproval", got)
	}
	if got := dataString(t, ok, "status"); got != domain.ScopeActive {
		t.Fatalf("status = %q, want active", got)
	}
}
