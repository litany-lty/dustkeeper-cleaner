// Package retention decides which release sets a host must keep, before any
// file is offered for cleaning.
//
// It exists because "keep only the newest" is not a statement about a file: it
// is a statement about a release set, and a set is only restorable when every
// component named by its manifest is present. The package therefore works from a
// declared manifest - applications, releases, components, live references - and
// never from a directory listing. A directory listing cannot tell a current
// client build from an obsolete one, and the version number is not a substitute:
// the client of this very installation is older than the server.
//
// Nothing here touches the filesystem and nothing here deletes. The result is a
// reviewed plan: what must stay, why, what is a candidate, and where the "keep
// exactly one set" goal was not met.
package retention

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"dustkeeper/internal/strictjson"
)

// Component is one file a release cannot be restored without.
type Component struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	// Version is the component's own version when it differs from the release.
	// It is informational: a set is kept or dropped as a whole, never per
	// component version.
	Version string `json:"version,omitempty"`
}

// Release is one publishable set.
type Release struct {
	ID          string      `json:"id"`
	PublishedAt string      `json:"published_at"`
	Published   bool        `json:"published"`
	Complete    bool        `json:"complete"`
	Active      bool        `json:"active"`
	InProgress  bool        `json:"restore_in_progress"`
	Components  []Component `json:"components"`
}

// Reference is a live use of a path: a running process, an enabled unit, a site
// configuration, a current pointer.
type Reference struct {
	Path   string `json:"path"`
	Why    string `json:"why"`
	Active bool   `json:"active"`
}

// App is one application and everything declared about it.
type App struct {
	Name        string      `json:"name"`
	Releases    []Release   `json:"releases"`
	References  []Reference `json:"references,omitempty"`
	Protections []Reference `json:"protections,omitempty"`
	Unresolved  []Reference `json:"unresolved,omitempty"`
}

// Manifest is the file an operator writes once per host.
type Manifest struct {
	Schema       int   `json:"schema"`
	Applications []App `json:"applications"`
}

// MaxManifestBytes bounds the declaration; an application list is not a dump.
const MaxManifestBytes = 1 << 20

// Reason codes. They are what the plan prints, and each one is a different
// answer to "why is this file still here".
const (
	ReasonReference      = "current_reference"
	ReasonLatest         = "latest_qualified_release"
	ReasonRestore        = "restore_in_progress"
	ReasonProtected      = "explicit_protection"
	ReasonUnresolved     = "unresolved_review_or_operation"
	ReasonIncomplete     = "incomplete_release_kept_for_inspection"
	ReasonSuperseded     = "superseded_release"
	ReasonComponentInUse = "component_still_referenced"
	ReasonSharedKeep     = "shared_path_kept_by_another_application"
)

// Kept is one path the plan keeps, with the reason that decides it.
type Kept struct {
	Path    string `json:"path"`
	Release string `json:"release,omitempty"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
}

// Candidate is one path the plan offers for cleaning. It is a proposal: none of
// the existing gates - model review, identity, protection, grants - is bypassed
// by appearing here.
type Candidate struct {
	Path    string `json:"path"`
	Release string `json:"release"`
	Why     string `json:"why"`
}

// AppPlan is the decision for one application.
type AppPlan struct {
	App             string      `json:"app"`
	LatestQualified string      `json:"latest_qualified_release,omitempty"`
	KeptSets        int         `json:"kept_release_sets"`
	Keep            []Kept      `json:"keep"`
	Candidates      []Candidate `json:"candidates"`
	Notes           []string    `json:"notes,omitempty"`
	// GoalMet is true only when exactly one qualified set is kept and nothing
	// outside it is retained. A plan with more kept sets says so rather than
	// claiming "only the newest remains".
	GoalMet bool `json:"goal_met"`
}

// Plan is the whole answer.
type Plan struct {
	Apps           []AppPlan `json:"apps"`
	KeptFiles      int       `json:"kept_files"`
	CandidateFiles int       `json:"candidate_files"`
	// SharedPaths are paths declared by more than one application. They are
	// reported rather than hidden: Build makes one application's keep conclusion
	// outrank every other application's proposal to clean the same file.
	SharedPaths []string `json:"shared_paths,omitempty"`
	// GoalMet is true only when every application met its goal.
	GoalMet bool `json:"goal_met"`
}

// Parse reads and validates a manifest. Every path must be absolute and clean.
//
// A path may legitimately be declared by more than one application - a shared
// library is the ordinary case. Refusing the whole manifest for that used to
// leave no plan at all, which is worse than a conservative plan: keep wins for
// shared paths in Build, and the declaration is still fully validated here.
func Parse(raw []byte) (Manifest, error) {
	if len(raw) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("manifest exceeds %d bytes", MaxManifestBytes)
	}
	opts := strictjson.Defaults()
	opts.MaxBytes = MaxManifestBytes
	opts.IntegerOnly = false
	var m Manifest
	if err := strictjson.Unmarshal(raw, &m, opts); err != nil {
		return Manifest{}, fmt.Errorf("manifest is not a valid declaration: %w", err)
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("manifest schema must be 1")
	}
	if len(m.Applications) == 0 {
		return Manifest{}, errors.New("manifest declares no applications")
	}
	names := map[string]bool{}
	for i := range m.Applications {
		app := &m.Applications[i]
		if strings.TrimSpace(app.Name) == "" {
			return Manifest{}, fmt.Errorf("application %d has no name", i)
		}
		if names[app.Name] {
			return Manifest{}, fmt.Errorf("application %q is declared twice", app.Name)
		}
		names[app.Name] = true
		if len(app.Releases) == 0 {
			return Manifest{}, fmt.Errorf("application %q declares no releases", app.Name)
		}
		seenRelease := map[string]bool{}
		for j := range app.Releases {
			rel := &app.Releases[j]
			if rel.ID == "" {
				return Manifest{}, fmt.Errorf("application %q has a release without an id", app.Name)
			}
			if seenRelease[rel.ID] {
				return Manifest{}, fmt.Errorf("application %q declares release %q twice", app.Name, rel.ID)
			}
			seenRelease[rel.ID] = true
			if _, err := time.Parse(time.RFC3339, rel.PublishedAt); err != nil {
				return Manifest{}, fmt.Errorf("release %s/%s has no RFC3339 published_at", app.Name, rel.ID)
			}
			if len(rel.Components) == 0 {
				return Manifest{}, fmt.Errorf("release %s/%s has no components", app.Name, rel.ID)
			}
			for _, c := range rel.Components {
				if err := checkPath(c.Path); err != nil {
					return Manifest{}, fmt.Errorf("release %s/%s: %w", app.Name, rel.ID, err)
				}
			}
		}
		for _, group := range [][]Reference{app.References, app.Protections, app.Unresolved} {
			for _, r := range group {
				if err := checkPath(r.Path); err != nil {
					return Manifest{}, err
				}
			}
		}
	}
	return m, nil
}

func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is not absolute", p)
	}
	if strings.HasSuffix(p, "/") || strings.Contains(p, "//") || strings.Contains(p, "/./") || strings.Contains(p, "/../") {
		return fmt.Errorf("path %q is not a clean file path", p)
	}
	return nil
}

// Build computes what each application must keep.
//
// Plans are computed per application first, then reconciled once across the
// whole manifest. The second step exists for one specific case: the same path
// belongs to two applications, one keeps it, and the other would offer it as a
// superseded component. "Keep" wins. A shared file that one service still runs
// is not a cleaning candidate for any other service, and a plan that quietly
// listed it on both sides would be unanswerable from the page.
func Build(m Manifest) Plan {
	out := Plan{GoalMet: true}
	apps := make([]AppPlan, 0, len(m.Applications))
	for _, app := range m.Applications {
		apps = append(apps, planApp(app))
	}

	shared := sharedPaths(m)
	for path := range shared {
		out.SharedPaths = append(out.SharedPaths, path)
	}
	sort.Strings(out.SharedPaths)

	// path -> the application whose keep decision outranks the other proposals.
	keepWinner := map[string]string{}
	for _, ap := range apps {
		for _, k := range ap.Keep {
			if _, ok := shared[k.Path]; ok && keepWinner[k.Path] == "" {
				keepWinner[k.Path] = ap.App
			}
		}
	}
	for i := range apps {
		ap := &apps[i]
		var remaining []Candidate
		for _, c := range ap.Candidates {
			owner, kept := keepWinner[c.Path]
			if !kept || owner == ap.App {
				remaining = append(remaining, c)
				continue
			}
			// The same path is kept in this application's list too, so "what is
			// on this host" and "what may be proposed" cannot disagree.
			ap.Keep = append(ap.Keep, Kept{Path: c.Path, Release: c.Release,
				Reason: ReasonSharedKeep, Detail: "kept by application " + owner})
			ap.GoalMet = false
			ap.Notes = append(ap.Notes, fmt.Sprintf(
				"shared path %s is kept by application %s; it is not offered for cleaning here", c.Path, owner))
		}
		ap.Candidates = remaining
		ap.Keep = sortedKeptSlice(ap.Keep)
		for _, c := range ap.Candidates {
			if owners, ok := shared[c.Path]; ok && keepWinner[c.Path] == "" {
				ap.Notes = append(ap.Notes, fmt.Sprintf(
					"shared path %s is declared by %d applications and no application keeps it; clarify ownership before cleaning",
					c.Path, len(owners)))
			}
		}
	}
	for _, ap := range apps {
		if !ap.GoalMet {
			out.GoalMet = false
		}
		out.KeptFiles += len(ap.Keep)
		out.CandidateFiles += len(ap.Candidates)
	}
	out.Apps = apps
	return out
}

// sharedPaths finds paths declared by more than one application. A path named
// twice by the same application is not shared; counting an application once per
// path keeps the result about ownership, not about how many releases list it.
func sharedPaths(m Manifest) map[string][]string {
	owners := map[string][]string{}
	for _, app := range m.Applications {
		seen := map[string]bool{}
		add := func(p string) {
			if seen[p] {
				return
			}
			seen[p] = true
			owners[p] = append(owners[p], app.Name)
		}
		for _, rel := range app.Releases {
			for _, c := range rel.Components {
				add(c.Path)
			}
		}
		for _, group := range [][]Reference{app.References, app.Protections, app.Unresolved} {
			for _, r := range group {
				add(r.Path)
			}
		}
	}
	out := map[string][]string{}
	for p, names := range owners {
		if len(names) > 1 {
			out[p] = names
		}
	}
	return out
}

func sortedKeptSlice(in []Kept) []Kept {
	sort.SliceStable(in, func(i, j int) bool { return in[i].Path < in[j].Path })
	return in
}

func planApp(app App) AppPlan {
	ap := AppPlan{App: app.Name}

	// Paths that must stay whatever the release list says. A live reference is
	// not an exception to the retention goal; it outranks it.
	mustKeep := map[string]Kept{}
	for _, r := range app.References {
		if r.Active {
			mustKeep[r.Path] = Kept{Path: r.Path, Reason: ReasonReference, Detail: r.Why}
		}
	}
	for _, p := range app.Protections {
		mustKeep[p.Path] = Kept{Path: p.Path, Reason: ReasonProtected, Detail: p.Why}
	}
	for _, u := range app.Unresolved {
		mustKeep[u.Path] = Kept{Path: u.Path, Reason: ReasonUnresolved, Detail: u.Why}
	}

	// A set is qualified when it was published successfully, the publisher says
	// it is complete, and every component carries the digest that proves it.
	var qualified []Release
	var incomplete []Release
	qualifiedID := map[string]bool{}
	for _, rel := range app.Releases {
		if rel.Published && rel.Complete && len(rel.Components) > 0 && allDigests(rel) {
			qualified = append(qualified, rel)
			qualifiedID[rel.ID] = true
			continue
		}
		incomplete = append(incomplete, rel)
	}
	sort.SliceStable(qualified, func(i, j int) bool { return newer(qualified[i], qualified[j]) })
	if len(qualified) > 0 {
		ap.LatestQualified = qualified[0].ID
	}

	keepSet := map[string]bool{} // release ids whose components are kept
	if ap.LatestQualified != "" {
		keepSet[ap.LatestQualified] = true
	}
	for _, rel := range app.Releases {
		if rel.Active || rel.InProgress {
			keepSet[rel.ID] = true
			if rel.ID != ap.LatestQualified {
				ap.Notes = append(ap.Notes,
					fmt.Sprintf("release %s is still %s, so it is kept alongside the latest qualified set",
						rel.ID, activeReason(rel)))
			}
		}
	}
	if len(incomplete) > 0 && ap.LatestQualified != "" {
		newest := newestRelease(app.Releases)
		for _, rel := range incomplete {
			if rel.ID == newest.ID {
				ap.Notes = append(ap.Notes, fmt.Sprintf(
					"release %s is the newest upload but is not usable (%s); the previous complete set %s is kept",
					rel.ID, whyIncomplete(rel), ap.LatestQualified))
			}
		}
	}

	for _, rel := range app.Releases {
		if keepSet[rel.ID] {
			for _, c := range rel.Components {
				reason := ReasonLatest
				if rel.Active || rel.InProgress {
					reason = ReasonRestore
					if rel.Active {
						reason = ReasonReference
					}
				}
				if existing, ok := mustKeep[c.Path]; ok {
					existing.Release = rel.ID
					mustKeep[c.Path] = existing
					continue
				}
				mustKeep[c.Path] = Kept{Path: c.Path, Release: rel.ID, Reason: reason}
			}
			continue
		}
		if !qualifiedID[rel.ID] {
			// An incomplete set is kept for inspection: deleting the only copy of
			// a failed upload destroys the evidence of what went wrong, and it is
			// not the set anyone will restore from.
			for _, c := range rel.Components {
				if _, ok := mustKeep[c.Path]; !ok {
					mustKeep[c.Path] = Kept{Path: c.Path, Release: rel.ID, Reason: ReasonIncomplete,
						Detail: whyIncomplete(rel)}
				}
			}
			continue
		}
		// A complete, superseded set: its components are candidates unless a
		// live reference or protection still names them.
		for _, c := range rel.Components {
			if k, ok := mustKeep[c.Path]; ok {
				k.Detail = strings.TrimSpace(k.Detail + "; " + ReasonComponentInUse + " of " + rel.ID)
				mustKeep[c.Path] = k
				continue
			}
			ap.Candidates = append(ap.Candidates, Candidate{Path: c.Path, Release: rel.ID,
				Why: ReasonSuperseded + ": superseded by " + ap.LatestQualified})
		}
	}

	ap.Keep = sortedKept(mustKeep)
	ap.KeptSets = len(keepSet)
	if ap.LatestQualified == "" {
		ap.Notes = append(ap.Notes, "no qualified release: nothing in this application can be offered for cleaning")
		for _, c := range ap.Candidates {
			ap.Notes = append(ap.Notes, "candidate withheld because no complete set exists to fall back to: "+c.Path)
		}
		ap.Candidates = nil
	}
	ap.GoalMet = ap.LatestQualified != "" && ap.KeptSets == 1 && len(ap.Keep) == len(componentsOf(qualified[0]))
	if !ap.GoalMet {
		ap.Notes = append(ap.Notes, fmt.Sprintf(
			"kept %d release set(s) and %d file(s): the goal of exactly one restorable set is not met",
			ap.KeptSets, len(ap.Keep)))
	}
	sort.SliceStable(ap.Candidates, func(i, j int) bool { return ap.Candidates[i].Path < ap.Candidates[j].Path })
	return ap
}

func allDigests(rel Release) bool {
	for _, c := range rel.Components {
		if c.SHA256 == "" {
			return false
		}
	}
	return true
}

func newer(a, b Release) bool {
	at, _ := time.Parse(time.RFC3339, a.PublishedAt)
	bt, _ := time.Parse(time.RFC3339, b.PublishedAt)
	if at.Equal(bt) {
		return a.ID < b.ID
	}
	return at.After(bt)
}

// newestRelease is the newest upload by time, whether or not it is usable. The
// "newest" question and the "latest qualified" question have different answers
// when the newest upload is broken, and conflating them is how the only complete
// set gets deleted.
func newestRelease(rels []Release) Release {
	if len(rels) == 0 {
		return Release{}
	}
	newest := rels[0]
	for _, rel := range rels[1:] {
		if newer(rel, newest) {
			newest = rel
		}
	}
	return newest
}

func activeReason(rel Release) string {
	if rel.Active && rel.InProgress {
		return "referenced by a current pointer and being restored"
	}
	if rel.Active {
		return "referenced by a current pointer"
	}
	return "being restored"
}

func whyIncomplete(rel Release) string {
	var why []string
	if !rel.Published {
		why = append(why, "the publish did not succeed")
	}
	if !rel.Complete {
		why = append(why, "the publisher did not declare it complete")
	}
	if !allDigests(rel) {
		why = append(why, "at least one component has no digest")
	}
	if len(why) == 0 {
		return "unknown"
	}
	return strings.Join(why, "; ")
}

func componentsOf(rel Release) []Component { return rel.Components }

func sortedKept(m map[string]Kept) []Kept {
	out := make([]Kept, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Marshal renders the plan for a machine reader.
func (p Plan) Marshal() ([]byte, error) { return json.MarshalIndent(p, "", "  ") }
