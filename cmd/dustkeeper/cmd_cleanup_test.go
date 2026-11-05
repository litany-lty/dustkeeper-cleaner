package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dustkeeper/internal/assess"
	"dustkeeper/internal/cleanup"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/report"
	"dustkeeper/internal/store"
)

// These tests are about the two halves of the one-time confirmation: the preview
// that writes nothing, and the confirmation that writes exactly what was
// previewed. Both are command-line contracts, so they go through dispatch rather
// than through internal/cleanup.

// cleanupFixture initialises a state directory with one registered scope.
func cleanupFixture(t *testing.T) (base, tree string) {
	t.Helper()
	base = newHome(t)
	tree = filepath.Join(base, "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	mustCLI(t, "--json", "init")
	mustCLI(t, "--json", "scope", "add", "--path", tree, "--kind", "managed_disposable",
		"--name", "exports")
	return base, tree
}

// withStore opens the test instance's database for assertions.
func withStore(t *testing.T, base string, fn func(st *store.Store)) {
	t.Helper()
	st, err := store.Open(filepath.Join(base, "state", "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	fn(st)
}

// storedPolicy reads back what a confirmation actually left behind.
func storedPolicy(t *testing.T, base string) cleanup.Policy {
	t.Helper()
	var pol cleanup.Policy
	withStore(t, base, func(st *store.Store) {
		p, err := cleanup.Load(context.Background(), st)
		if err != nil {
			t.Fatalf("load policy: %v", err)
		}
		pol = p
	})
	return pol
}

// eventCount is how many events the instance has recorded.
//
// It is the cheapest observable proof that a command took no action: every
// action, every refusal and every notification writes exactly one event, and no
// read-only command writes any.
func eventCount(t *testing.T, base string) int {
	t.Helper()
	n := 0
	withStore(t, base, func(st *store.Store) {
		events, err := st.ListEvents(context.Background(), 10000, 0)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		n = len(events)
	})
	return n
}

// TestSetupPreviewChangesNothing is the first half of the two-step confirmation.
//
// The judgment is narrow and absolute: a preview that changed state would make
// the digest meaningless, because the thing the user approved would no longer be
// the thing that gets written.
func TestSetupPreviewChangesNothing(t *testing.T) {
	base, _ := cleanupFixture(t)
	before := eventCount(t, base)

	r := mustCLI(t, "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatArchive, "--observation-days", "30")

	// A person has to be able to check the five answers without reading JSON.
	for _, want := range []string{
		"nothing has been written yet",
		"exports",
		assess.CatArchive,
		"30 天",
		"macos_notification",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the preview does not show %q:\n%s", want, r.stdout)
		}
	}
	// And the confirmation must be one copy and paste away from the preview.
	if !strings.Contains(r.stdout, "--digest ") {
		t.Errorf("the preview does not print a confirmation command:\n%s", r.stdout)
	}

	pol := storedPolicy(t, base)
	if pol.Confirmed() {
		t.Error("a preview confirmed the policy")
	}
	if pol.ObservationDays == 30 {
		t.Error("a preview wrote the observation window into the policy")
	}
	if got := eventCount(t, base); got != before {
		t.Errorf("the preview recorded %d events, want none", got-before)
	}
}

// TestThePreviewCarriesTheDigestItAsksToConfirm checks the machine-readable half:
// a script previews, reads the digest, and confirms without parsing prose.
func TestThePreviewCarriesTheDigestItAsksToConfirm(t *testing.T) {
	cleanupFixture(t)

	r := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports")

	digest := dataString(t, r, "digest")
	if !strings.HasPrefix(digest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", digest)
	}
	hint := dataString(t, r, "confirm_hint")
	if !strings.Contains(hint, digest) {
		t.Errorf("the hint %q does not carry the digest %q", hint, digest)
	}
	if !strings.Contains(hint, "--scopes exports") {
		t.Errorf("the hint %q would not reproduce the same policy", hint)
	}
}

// TestAnEmptyScopeListSaysWhatItMeans covers the worst misreading available on
// this screen: "管理目录: （无）" can be read as "no restriction" when it in fact
// means "no directory at all", because Covers() answers no for an empty list.
func TestAnEmptyScopeListSaysWhatItMeans(t *testing.T) {
	cleanupFixture(t)

	r := mustCLI(t, "cleanup", "setup")

	if !strings.Contains(r.stdout, "不覆盖任何目录") {
		t.Errorf("the preview does not say an empty scope list covers nothing:\n%s", r.stdout)
	}
	// A pasted confirmation should not carry flags whose value is an empty
	// string: the policy is identical without them, and `--scopes ''` reads
	// like a typo in a command the user is told to run.
	if strings.Contains(r.stdout, "''") || strings.Contains(r.stdout, `""`) {
		t.Errorf("the confirmation command quotes an empty list:\n%s", r.stdout)
	}
}

// TestTheDefaultPreviewConfirmsIntoTheDefaultPolicy closes the loop for the bare
// invocation: the command the preview prints must actually work.
func TestTheDefaultPreviewConfirmsIntoTheDefaultPolicy(t *testing.T) {
	base, _ := cleanupFixture(t)

	preview := mustCLI(t, "--json", "cleanup", "setup")
	digest := dataString(t, preview, "digest")
	if hint := dataString(t, preview, "confirm_hint"); !strings.Contains(hint, digest) {
		t.Fatalf("the hint %q does not carry the digest", hint)
	}

	mustCLI(t, "--json", "cleanup", "setup", "--digest", digest)

	pol := storedPolicy(t, base)
	if !pol.Confirmed() {
		t.Fatal("the default preview could not be confirmed with its own digest")
	}
	if got := len(pol.Scopes); got != 0 {
		t.Errorf("scopes = %v, want none", pol.Scopes)
	}
	if pol.Covers("exports") {
		t.Error("an empty scope list covers a scope")
	}
}

// TestConfirmingThePreviewedPolicyWritesExactlyIt closes the loop.
func TestConfirmingThePreviewedPolicyWritesExactlyIt(t *testing.T) {
	base, _ := cleanupFixture(t)

	preview := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatArchive, "--observation-days", "21")
	digest := dataString(t, preview, "digest")

	confirmed := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatArchive, "--observation-days", "21", "--digest", digest)
	if got := dataString(t, confirmed, "digest"); got != digest {
		t.Errorf("the written digest is %q, want the previewed %q", got, digest)
	}

	pol := storedPolicy(t, base)
	if !pol.Confirmed() {
		t.Fatal("the policy is still unconfirmed")
	}
	if pol.Digest != digest {
		t.Errorf("stored digest = %q, want %q", pol.Digest, digest)
	}
	if pol.ObservationDays != 21 {
		t.Errorf("observation_days = %d, want 21", pol.ObservationDays)
	}
	if !pol.Covers("exports") || !pol.MayDeleteDirectly(assess.CatArchive) {
		t.Errorf("the confirmed policy does not carry what was previewed: %+v", pol)
	}
}

// TestAChangedPolicyCannotBeConfirmedWithAnOldDigest is the reason the confirmation
// carries a digest at all: the flag that is checked must be the content the user
// read, not merely the name of a command.
func TestAChangedPolicyCannotBeConfirmedWithAnOldDigest(t *testing.T) {
	base, _ := cleanupFixture(t)

	preview := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatArchive, "--observation-days", "14")
	digest := dataString(t, preview, "digest")

	// Same command, wider authority: 30 days instead of 14.
	r := cli(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatArchive, "--observation-days", "30", "--digest", digest)
	if r.code != 4 {
		t.Fatalf("exit = %d, want 4; stdout:\n%s", r.code, r.stdout)
	}
	if got := r.str(t, "code"); got != "DIGEST_MISMATCH" {
		t.Errorf("code = %q, want DIGEST_MISMATCH", got)
	}
	if storedPolicy(t, base).Confirmed() {
		t.Error("a mismatched confirmation wrote the policy anyway")
	}
}

// TestNoDirectDeleteCategoryMeansNothingIsDeletedDirectly pins the empty-list
// meaning. An empty list is not "everything": it is the default, and it has to
// answer "no" for every category the classifier knows.
func TestNoDirectDeleteCategoryMeansNothingIsDeletedDirectly(t *testing.T) {
	base, _ := cleanupFixture(t)

	preview := mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports")
	mustCLI(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--digest", dataString(t, preview, "digest"))

	pol := storedPolicy(t, base)
	if len(pol.DirectDeleteCategories) != 0 {
		t.Fatalf("the confirmed policy allows direct deletion of %v", pol.DirectDeleteCategories)
	}
	for _, cat := range assess.KnownCategories() {
		if pol.MayDeleteDirectly(cat) {
			t.Errorf("category %q may be deleted directly without being named", cat)
		}
	}
	if pol.ObservationDays < cleanup.MinObservationDays {
		t.Errorf("observation_days = %d, want at least %d", pol.ObservationDays, cleanup.MinObservationDays)
	}
}

// TestDirectDeletionNeedsNames covers the two refusals that keep an approval from
// meaning more than it says.
func TestDirectDeletionNeedsNames(t *testing.T) {
	cleanupFixture(t)

	// Categories without scopes: an approval of "here" with no "where".
	r := cli(t, "--json", "cleanup", "setup", "--categories", assess.CatArchive)
	if r.code != 2 {
		t.Fatalf("exit = %d, want 2; stdout:\n%s", r.code, r.stdout)
	}
	if got := r.str(t, "code"); got != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want INVALID_ARGUMENT", got)
	}
	if !strings.Contains(strings.ToLower(r.str(t, "message")), "--scopes") {
		t.Errorf("the refusal does not say which flag is missing: %q", r.str(t, "message"))
	}

	// A scope that was never registered: approving a directory this program
	// cannot see would be approving nothing while looking like approving something.
	r = cli(t, "--json", "cleanup", "setup", "--scopes", "elsewhere")
	if r.code != 2 {
		t.Fatalf("exit = %d, want 2; stdout:\n%s", r.code, r.stdout)
	}
	if !strings.Contains(r.str(t, "message"), "scope add") {
		t.Errorf("the refusal does not say how to fix it: %q", r.str(t, "message"))
	}

	// A protective category may never be authorised for direct deletion.
	r = cli(t, "--json", "cleanup", "setup", "--scopes", "exports",
		"--categories", assess.CatCredential)
	if r.code != 2 {
		t.Fatalf("exit = %d, want 2; stdout:\n%s", r.code, r.stdout)
	}
	if !strings.Contains(r.str(t, "message"), assess.CatCredential) {
		t.Errorf("the refusal does not name the category: %q", r.str(t, "message"))
	}
}

// TestReportBeforeAnyRunSaysSo covers the empty installation: no page yet is not
// an error, it is a fact about a program that has not run.
func TestReportBeforeAnyRunSaysSo(t *testing.T) {
	base, _ := cleanupFixture(t)

	r := cli(t, "--json", "cleanup", "report")
	if r.code != 0 {
		t.Fatalf("exit = %d, want 0; stdout:\n%s", r.code, r.stdout)
	}
	if got := r.str(t, "code"); got != "NO_CANDIDATES" {
		t.Errorf("code = %q, want NO_CANDIDATES", got)
	}
	if !strings.Contains(r.str(t, "message"), filepath.Join(base, "state", "reports")) {
		t.Errorf("the message does not say where the page would be: %q", r.str(t, "message"))
	}
}

// TestARunWithNothingToDoStillLeavesAPage is the "fixed place" promise: a user
// told to look at the result page must find a page, not yesterday's page.
func TestARunWithNothingToDoStillLeavesAPage(t *testing.T) {
	base, _ := cleanupFixture(t)

	run := mustCLI(t, "run")
	if !strings.Contains(run.stdout, "this run: 0 deleted, 0 moved to the observation area, 0 left alone") {
		t.Errorf("the run does not summarise itself in one line:\n%s", run.stdout)
	}
	page := filepath.Join(base, "state", "reports", report.LatestMarkdown)
	if !strings.Contains(run.stdout, page) {
		t.Errorf("the run does not say where the page is:\n%s", run.stdout)
	}

	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("the run left no page at %s: %v", page, err)
	}
	for _, want := range []string{"本轮清理结果", "已删除（0）", "待删除（0）", "保留（0）"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the page does not say %q:\n%s", want, raw)
		}
	}

	// The reading command and the writing run must agree on one file.
	printed := mustCLI(t, "cleanup", "report")
	if strings.TrimSpace(printed.stdout) != strings.TrimSpace(string(raw)) {
		t.Errorf("`cleanup report` printed something else than the page it points at:\n%s", printed.stdout)
	}
}

// TestThePageDescribesThisRunOnly is the window rule.
//
// An object isolated an hour ago belongs to the run that isolated it. A run that
// touches nothing an hour later must not present that object as its own work -
// otherwise "this run deleted nothing" and "this run deleted five files" become
// the same page every time the program wakes up.
func TestThePageDescribesThisRunOnly(t *testing.T) {
	clk := domain.NewFakeClock(time.Now())
	base, tree := scopeWithRule(t, clk)
	installClockReviewFixture(t, base, tree, clk)
	created := mustCLIOn(t, clk, "plan", "create", "--rule", "r1", "--json")
	mustCLIOn(t, clk, "plan", "apply", "--id", dataString(t, created, "plan_id"),
		"--digest", dataString(t, created, "digest"))

	// The next run happens later, and does nothing: the object is observed, not
	// due, and the tree holds nothing new.
	clk.Advance(time.Hour)
	ran := mustCLIOn(t, clk, "run")

	if !strings.Contains(ran.stdout, "this run: 0 deleted, 0 moved to the observation area, 0 left alone") {
		t.Errorf("the page counts the previous run's work as its own:\n%s", ran.stdout)
	}
	raw, err := os.ReadFile(filepath.Join(base, "state", "reports", report.LatestMarkdown))
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	if !strings.Contains(string(raw), "本次没有文件进入待删除区") {
		t.Errorf("the page describes the earlier quarantine:\n%s", raw)
	}
}

// TestReportPrintsThePageAndActsOnNothing is the read-only contract.
func TestReportPrintsThePageAndActsOnNothing(t *testing.T) {
	base, tree := cleanupFixture(t)
	victim := filepath.Join(tree, "old.zip")
	if err := os.WriteFile(victim, []byte("payload"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dir := filepath.Join(base, "state", "reports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir reports: %v", err)
	}
	page := "# 本次清理结果\n\n## 已删除\n\n- old.zip\n"
	if err := os.WriteFile(filepath.Join(dir, report.LatestMarkdown), []byte(page), 0o600); err != nil {
		t.Fatalf("write page: %v", err)
	}
	raw := `{"deleted":{"entries":[],"total":1,"more":0}}`
	if err := os.WriteFile(filepath.Join(dir, report.LatestJSON), []byte(raw), 0o600); err != nil {
		t.Fatalf("write page json: %v", err)
	}

	before := eventCount(t, base)
	human := mustCLI(t, "cleanup", "report")
	if !strings.Contains(human.stdout, "old.zip") {
		t.Errorf("the report does not print the page:\n%s", human.stdout)
	}

	machine := mustCLI(t, "--json", "cleanup", "report")
	if got := dataString(t, machine, "path"); got != filepath.Join(dir, report.LatestMarkdown) {
		t.Errorf("path = %q", got)
	}
	if !strings.Contains(dataString(t, machine, "page"), "deleted") {
		t.Error("--json should carry the page as data, not only as text")
	}

	// Nothing moved, nothing was recorded, the candidate is still there.
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the candidate is gone after a read-only command: %v", err)
	}
	if got := eventCount(t, base); got != before {
		t.Errorf("the report recorded %d events, want none", got-before)
	}
	again, err := os.ReadFile(filepath.Join(dir, report.LatestMarkdown))
	if err != nil {
		t.Fatalf("read page again: %v", err)
	}
	if string(again) != page {
		t.Error("the report rewrote the page")
	}
}
