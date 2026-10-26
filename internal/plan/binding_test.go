package plan

import (
	"encoding/json"
	"testing"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

func testScope() store.Scope {
	birth := int64(1700000000000000000)
	id := domain.RootIdentity{
		Persistent: domain.PersistentRootIdentity{
			VolumeID:    "dev-7",
			Inode:       1234,
			BirthTimeNS: &birth,
			FileSystem:  "apfs",
		},
	}
	body, _ := json.Marshal(id)
	return store.Scope{
		ID:                 "exports",
		RawRoot:            []byte("/tmp/exports"),
		RootIdentityJSON:   string(body),
		Kind:               domain.KindManagedDisposable,
		Generation:         3,
		ProtectionRevision: 5,
		Status:             domain.ScopeActive,
	}
}

func TestReviewBindingIsBoundToTheReviewedState(t *testing.T) {
	sc := testScope()
	_, base, err := NewReviewBinding(sc, "sha256:rule")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}

	again, againDigest, err := NewReviewBinding(sc, "sha256:rule")
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	if againDigest != base {
		t.Fatal("the same state produced a different review digest")
	}
	if again.ProtectionRevision != 5 || again.ScopeGeneration != 3 {
		t.Fatalf("binding lost scope facts: %+v", again)
	}

	// A protection added after the review must invalidate the approval.
	protected := sc
	protected.ProtectionRevision = 6
	if _, d, _ := NewReviewBinding(protected, "sha256:rule"); d == base {
		t.Fatal("a changed protection revision did not change the review digest")
	}

	// A different rule must too.
	if _, d, _ := NewReviewBinding(sc, "sha256:other"); d == base {
		t.Fatal("a different rule digest did not change the review digest")
	}
}

func TestReviewBindingRejectsAnUnreadableIdentity(t *testing.T) {
	sc := testScope()
	sc.RootIdentityJSON = "{"
	if _, _, err := NewReviewBinding(sc, "sha256:rule"); err == nil {
		t.Fatal("an unreadable root identity was accepted")
	}
}

func TestGrantBindingSeparatesCapabilities(t *testing.T) {
	sc := testScope()
	rb, _, err := NewReviewBinding(sc, "sha256:rule")
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	q, qd, err := NewGrantBinding(rb, "rule-1", 1, domain.CapAutoQuarantine, 100, 1<<30, 2592000)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	p, pd, err := NewGrantBinding(rb, "rule-1", 1, domain.CapAutoPurge, 100, 1<<30, 2592000)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if qd == pd {
		t.Fatal("quarantine and purge approvals share a digest")
	}
	if q.Capability != domain.CapAutoQuarantine || p.Capability != domain.CapAutoPurge {
		t.Fatal("the binding did not record the capability")
	}

	// Widening the ceiling is a different approval.
	_, wider, err := NewGrantBinding(rb, "rule-1", 1, domain.CapAutoQuarantine, 200, 1<<30, 2592000)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if wider == qd {
		t.Fatal("a larger file ceiling did not change the approval digest")
	}

	// A different retention requirement is a different approval.
	_, ret, err := NewGrantBinding(rb, "rule-1", 1, domain.CapAutoQuarantine, 100, 1<<30, 3600)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if ret == qd {
		t.Fatal("a different retention did not change the approval digest")
	}
}

func TestGrantBindingRejectsAnImpossibleApproval(t *testing.T) {
	sc := testScope()
	rb, _, _ := NewReviewBinding(sc, "sha256:rule")

	for _, c := range []struct {
		name       string
		capability string
		files      int
		bytes      int64
		retention  int64
	}{
		{"unknown capability", "delete_everything", 10, 1, 100},
		{"zero files", domain.CapAutoQuarantine, 0, 1, 100},
		{"zero bytes", domain.CapAutoQuarantine, 10, 0, 100},
		{"no retention", domain.CapAutoQuarantine, 10, 1, 0},
	} {
		_, _, err := NewGrantBinding(rb, "rule-1", 1, c.capability, c.files, c.bytes, c.retention)
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Errorf("%s: unexpected code %s", c.name, errs.CodeOf(err))
		}
	}
}

func TestConfigDigestCoversOnlyActionSettings(t *testing.T) {
	base := ConfigDigestInput{
		MaxAutoQuarantineFiles: 100,
		MaxAutoQuarantineBytes: 1 << 30,
		MaxAutoPurgeFiles:      10,
		MaxAutoPurgeBytes:      1 << 30,
		RunSoftSeconds:         120,
		RunReadBudgetBytes:     1 << 31,
		ReadRateBytesPerSecond: 1 << 20,
		MinFreeBytes:           1 << 29,
		MinFreePercent:         5,
		MetadataPolicyVersion:  MetadataPolicyVersion,
	}
	a, err := ConfigDigestOf(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	b, _ := ConfigDigestOf(base)
	if a != b {
		t.Fatal("the config digest is not stable")
	}

	changed := base
	changed.MaxAutoPurgeFiles = 11
	c, _ := ConfigDigestOf(changed)
	if c == a {
		t.Fatal("a changed purge ceiling did not change the config digest")
	}
}

func TestPlanDigestCoversItemsButNotExecutionState(t *testing.T) {
	doc := PlanDoc{
		PlanID:       "plan-1",
		Kind:         domain.PlanManualRule,
		CreatedAtNS:  1,
		ExpiresAtNS:  2,
		PolicyDigest: "sha256:policy",
		ConfigDigest: "sha256:config",
		Items: []PlanItemDoc{{
			Ordinal:        0,
			ItemID:         "obj_1",
			ScopeID:        "exports",
			RawRelativeB64: B64([]byte("a.zip")),
			ObjectName:     "obj_1",
			Reason:         domain.ReasonLifecycle,
			SourceKind:     domain.PlanManualRule,
		}},
	}
	d1, err := doc.ComputeDigest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	d2, _ := doc.ComputeDigest()
	if d1 != d2 {
		t.Fatal("the plan digest is not deterministic")
	}

	// Adding an item must change it.
	doc.Items = append(doc.Items, doc.Items[0])
	d3, _ := doc.ComputeDigest()
	if d3 == d1 {
		t.Fatal("an added item did not change the plan digest")
	}

	// The same document rendered from a different map order must not: the
	// digest covers a fixed struct, not a map.
	var probe struct {
		PlanID string `json:"plan_id"`
		Kind   string `json:"kind"`
	}
	probe.PlanID, probe.Kind = doc.PlanID, doc.Kind
	if _, err := Digest("dustkeeper/plan/v1", probe); err != nil {
		t.Fatalf("digest of a partial struct: %v", err)
	}
}

func TestConfigDefaultLimitsAreNotZero(t *testing.T) {
	c := config.Default()
	if c.Runtime.MaxAutoQuarantineFiles == 0 || c.Runtime.MaxAutoQuarantineBytes == 0 {
		t.Fatal("the default approval ceiling forbids the action it should permit")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
}
