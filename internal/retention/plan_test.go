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

// A shared path is not a reason to refuse the whole manifest. It is a reason to
// let one application's keep conclusion outrank another's proposal: one file,
// two owners, and the one that still runs it decides for both.
func TestASharedPathIsKeptWhenEitherApplicationKeepsIt(t *testing.T) {
	raw := []byte(`{"schema":1,"applications":[
	  {"name":"a","releases":[
	    {"id":"old","published_at":"2026-01-01T00:00:00Z","published":true,"complete":true,
	      "components":[{"path":"/srv/shared.jar","sha256":"x"}]},
	    {"id":"new","published_at":"2026-02-01T00:00:00Z","published":true,"complete":true,
	      "components":[{"path":"/srv/a-new.jar","sha256":"y"}]}],
	   "references":[{"path":"/srv/shared.jar","why":"a.service is running it","active":true}]},
	  {"name":"b","releases":[
	    {"id":"old","published_at":"2026-01-01T00:00:00Z","published":true,"complete":true,
	      "components":[{"path":"/srv/shared.jar","sha256":"x"}]},
	    {"id":"new","published_at":"2026-03-01T00:00:00Z","published":true,"complete":true,
	      "components":[{"path":"/srv/b-new.jar","sha256":"z"}]}]}]}`)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("a shared path made the whole manifest unusable: %v", err)
	}
	plan := Build(m)
	if len(plan.SharedPaths) != 1 || plan.SharedPaths[0] != "/srv/shared.jar" {
		t.Fatalf("the shared path was not reported: %v", plan.SharedPaths)
	}
	for _, ap := range plan.Apps {
		for _, c := range ap.Candidates {
			if c.Path == "/srv/shared.jar" {
				t.Fatalf("application %s offered a shared path that another application keeps", ap.App)
			}
		}
		found := false
		for _, k := range ap.Keep {
			if k.Path == "/srv/shared.jar" {
				found = true
			}
		}
		if !found {
			t.Fatalf("application %s did not record the shared path as kept", ap.App)
		}
		if ap.GoalMet {
			t.Fatalf("application %s claims exactly one set while a shared file keeps another", ap.App)
		}
	}
	// The application that proposed it now says why it was withdrawn.
	b := plan.Apps[1]
	if !strings.Contains(strings.Join(b.Notes, "\n"), "kept by application a") {
		t.Fatalf("the withdrawn proposal was not explained: %v", b.Notes)
	}
}

// With no keep conclusion anywhere, the ambiguity is stated rather than hidden
// and the proposal remains a proposal (all the existing gates still apply).
func TestASharedPathNobodyKeepsIsCalledOut(t *testing.T) {
	m := Manifest{Schema: 1, Applications: []App{
		{Name: "a", Releases: []Release{
			{ID: "old", PublishedAt: "2026-01-01T00:00:00Z", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/shared.jar", SHA256: "x"}}},
			{ID: "new", PublishedAt: "2026-02-01T00:00:00Z", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/a-new.jar", SHA256: "y"}}},
		}},
		{Name: "b", Releases: []Release{
			{ID: "old", PublishedAt: "2026-01-01T00:00:00Z", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/shared.jar", SHA256: "x"}}},
			{ID: "new", PublishedAt: "2026-03-01T00:00:00Z", Published: true, Complete: true,
				Components: []Component{{Path: "/srv/b-new.jar", SHA256: "z"}}},
		}},
	}}
	plan := Build(m)
	for _, ap := range plan.Apps {
		if !strings.Contains(strings.Join(ap.Notes, "\n"), "clarify ownership before cleaning") {
			t.Fatalf("application %s hid the shared-path ambiguity: %v", ap.App, ap.Notes)
		}
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
