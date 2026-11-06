package retention

import (
	"strings"
	"testing"
)

// The two fixtures below are the two failures this package exists to prevent.
// Both are declarative: no filesystem, no model, no deletion.

// TestAnIncompleteNewestUploadKeepsThePreviousCompleteSet is V6.
//
// The newest upload is missing a component and the older complete set is still
// referenced by the running service. Keeping "the newest" would delete the only
// restorable set; deleting the referenced component would break the service.
func TestAnIncompleteNewestUploadKeepsThePreviousCompleteSet(t *testing.T) {
	m := Manifest{Schema: 1, Applications: []App{{
		Name: "controller",
		Releases: []Release{
			{ID: "v1.0.88", PublishedAt: "2026-10-20T10:00:00+08:00", Published: true, Complete: true,
				Active: true,
				Components: []Component{
					{Path: "/opt/app/releases/v1.0.88/app.jar", SHA256: "aa"},
					{Path: "/opt/app/releases/v1.0.88/conf.yml", SHA256: "bb"},
				}},
			// Newest upload, broken: one component has no digest.
			{ID: "v1.0.89", PublishedAt: "2026-11-05T09:00:00+08:00", Published: true, Complete: true,
				Components: []Component{
					{Path: "/opt/app/releases/v1.0.89/app.jar", SHA256: "cc"},
					{Path: "/opt/app/releases/v1.0.89/conf.yml"},
				}},
		},
		References: []Reference{
			{Path: "/opt/app/releases/v1.0.88/app.jar", Why: "controller.service is running it", Active: true},
		},
	}}}

	plan := Build(m)
	if len(plan.Apps) != 1 {
		t.Fatalf("apps = %d", len(plan.Apps))
	}
	ap := plan.Apps[0]
	if ap.LatestQualified != "v1.0.88" {
		t.Fatalf("latest qualified = %q; an incomplete upload cannot be the latest usable set", ap.LatestQualified)
	}
	if ap.GoalMet {
		t.Fatal("the plan claims exactly one restorable set while two are kept")
	}
	kept := map[string]Kept{}
	for _, k := range ap.Keep {
		kept[k.Path] = k
	}
	if _, ok := kept["/opt/app/releases/v1.0.88/conf.yml"]; !ok {
		t.Fatal("the complete set was broken: a component of the kept release is missing")
	}
	if _, ok := kept["/opt/app/releases/v1.0.89/app.jar"]; !ok {
		t.Fatal("the failed upload was discarded instead of kept for inspection")
	}
	for _, c := range ap.Candidates {
		if strings.Contains(c.Path, "v1.0.88") {
			t.Fatalf("a component of the kept set was offered for cleaning: %s", c.Path)
		}
	}
	joined := strings.Join(ap.Notes, "\n")
	if !strings.Contains(joined, "not usable") || !strings.Contains(joined, "v1.0.88") {
		t.Fatalf("the plan does not explain why the older set is kept:\n%s", joined)
	}
}

// TestARestoreSetIsKeptWholeAcrossDifferentComponentVersions is V7.
//
// A recovery backup is one set made of before/after snapshots and files from two
// machines. Their internal versions differ, and the client is deliberately older
// than the server. Keeping "the highest version" would split the set.
func TestARestoreSetIsKeptWholeAcrossDifferentComponentVersions(t *testing.T) {
	m := Manifest{Schema: 1, Applications: []App{{
		Name: "elidli",
		Releases: []Release{
			{ID: "v1.0.89-restore", PublishedAt: "2026-10-25T12:00:00+08:00", Published: true, Complete: true,
				InProgress: true,
				Components: []Component{
					{Path: "/opt/backup/v1.0.89/server.before.jar", SHA256: "1", Version: "1.0.89"},
					{Path: "/opt/backup/v1.0.89/server.after.jar", SHA256: "2", Version: "1.0.89"},
					{Path: "/opt/backup/v1.0.89/client.jar", SHA256: "3", Version: "1.0.46"},
					{Path: "/opt/backup/v1.0.89/edge.jar", SHA256: "4", Version: "1.0.62"},
				}},
		},
	}}}

	plan := Build(m)
	ap := plan.Apps[0]
	if len(ap.Keep) != 4 {
		t.Fatalf("kept %d file(s) of a four-component restore set: %+v", len(ap.Keep), ap.Keep)
	}
	if len(ap.Candidates) != 0 {
		t.Fatalf("a restore set in progress produced %d candidate(s)", len(ap.Candidates))
	}
	for _, k := range ap.Keep {
		if k.Release != "v1.0.89-restore" {
			t.Fatalf("component %s is not attributed to the set it belongs to: %+v", k.Path, k)
		}
	}
}

// TestAClientReferencedByAnOldPointerIsNotACandidate covers the case that made
// "keep the newest by number" wrong: the client pointer is older than the server
// and is still in use.
func TestAClientReferencedByAnOldPointerIsNotACandidate(t *testing.T) {
	m := Manifest{Schema: 1, Applications: []App{{
		Name: "site",
		Releases: []Release{
			{ID: "v1.0.89", PublishedAt: "2026-10-01T10:00:00+08:00", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/v1.0.89/app.jar", SHA256: "a"}}},
			{ID: "v1.0.46", PublishedAt: "2026-09-01T10:00:00+08:00", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/v1.0.46/client.jar", SHA256: "b"}}},
		},
		References: []Reference{{Path: "/srv/v1.0.46/client.jar", Why: "the site points at it", Active: true}},
	}}}
	ap := Build(m).Apps[0]
	for _, c := range ap.Candidates {
		if c.Path == "/srv/v1.0.46/client.jar" {
			t.Fatal("a live client reference was offered as a candidate because its version number is smaller")
		}
	}
	if len(ap.Keep) != 2 {
		t.Fatalf("kept %d file(s), want the newest set and the live client", len(ap.Keep))
	}
	if ap.GoalMet {
		t.Fatal("the plan claims one set while a live reference keeps a second")
	}
}

// TestAPathDeclaredByTwoApplicationsIsRefused keeps "may this be dropped"
// answerable: one file, two owners, and neither can answer for the other.
func TestAPathDeclaredByTwoApplicationsIsRefused(t *testing.T) {
	raw := []byte(`{"schema":1,"applications":[
	  {"name":"a","releases":[{"id":"1","published_at":"2026-01-01T00:00:00Z","published":true,"complete":true,
	    "components":[{"path":"/srv/shared.jar","sha256":"x"}]}]},
	  {"name":"b","releases":[{"id":"1","published_at":"2026-01-01T00:00:00Z","published":true,"complete":true,
	    "components":[{"path":"/srv/shared.jar","sha256":"x"}]}]}]}`)
	if _, err := Parse(raw); err == nil {
		t.Fatal("a path declared by two applications was accepted")
	}
}

// TestNoQualifiedSetWithholdsEveryCandidate is the conservative floor: without a
// complete set to fall back to, nothing in that application is proposed.
func TestNoQualifiedSetWithholdsEveryCandidate(t *testing.T) {
	m := Manifest{Schema: 1, Applications: []App{{
		Name: "only-broken",
		Releases: []Release{
			{ID: "v1", PublishedAt: "2026-10-01T10:00:00+08:00", Published: true, Complete: false,
				Components: []Component{{Path: "/srv/v1/a.jar", SHA256: "a"}}},
			{ID: "v2", PublishedAt: "2026-09-01T10:00:00+08:00", Published: false, Complete: true,
				Components: []Component{{Path: "/srv/v2/a.jar", SHA256: "b"}}},
		},
	}}}
	ap := Build(m).Apps[0]
	if len(ap.Candidates) != 0 {
		t.Fatalf("proposed %d candidate(s) while no restorable set exists", len(ap.Candidates))
	}
	if ap.LatestQualified != "" || ap.GoalMet {
		t.Fatalf("no qualified release should be reported: %+v", ap)
	}
	if !strings.Contains(strings.Join(ap.Notes, "\n"), "no qualified release") {
		t.Fatalf("the plan does not explain the refusal: %v", ap.Notes)
	}
}
