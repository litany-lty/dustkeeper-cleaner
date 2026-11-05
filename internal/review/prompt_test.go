package review

import (
	"strings"
	"testing"

	"dustkeeper/internal/domain"
)

// The three outcomes depend on the reviewer being able to *choose* keep. Proof
// that a file is still needed is exactly what the facts boundary forbids
// inventing, so keep has to be described as a resemblance, not as proof.
// Without this, the reviewer can only answer proceed or insufficient and the
// third outcome never fires.
func TestEveryActionExplainsWhenToKeep(t *testing.T) {
	for _, action := range []string{domain.DispositionDelete, domain.ActionQuarantine, domain.ActionPurge} {
		described, err := describeAction(action)
		if err != nil {
			t.Fatalf("describeAction(%q): %v", action, err)
		}
		for name, text := range map[string]string{
			"allow": described.allow, "insufficient": described.insufficient, "keep": described.keep,
		} {
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s/%s is empty: a choice without a description cannot be chosen", action, name)
			}
		}
		if !strings.Contains(described.keep, "looks like") {
			t.Errorf("%s/keep = %q; it requires proof instead of a resemblance", action, described.keep)
		}
		if !strings.Contains(described.question, "may never justify proceeding") {
			t.Errorf("%s question does not carry the boundary:\n%s", action, described.question)
		}
	}
}

// The asymmetry, stated once: a guess may save a file and may never act on one.
// This is the property that makes a cautious reviewer cheaper than a bold one.
func TestAGuessMaySaveButNeverAct(t *testing.T) {
	if !strings.Contains(factsBoundary, "may never justify proceeding") {
		t.Error("the boundary permits an inference to justify proceeding")
	}
	if !strings.Contains(factsBoundary, "may justify keeping the file") {
		t.Error("the boundary forbids keeping on a resemblance")
	}
}

// The shared text is read by both reviewers, so it must not contain one
// reviewer's answer vocabulary: MiniMax once received the word allow_review and
// that is indistinguishable from receiving JEV's answer.
func TestTheSharedBoundaryNamesNeitherVocabulary(t *testing.T) {
	for _, word := range []string{"allow_review", "deny", "reason_code"} {
		if strings.Contains(factsBoundary, word) {
			t.Errorf("the shared boundary carries the reviewer vocabulary %q", word)
		}
	}
}
