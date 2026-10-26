package policy

import (
	"testing"
	"time"

	"dustkeeper/internal/domain"
	"dustkeeper/internal/rules"
)

// TestPatternLanguage pins the deliberately tiny matcher: literals, '*' and '?'.
func TestPatternLanguage(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.zip", "a.zip", true},
		{"*.zip", "a.zip.txt", false},
		{"*.zip", ".zip", true},
		{"a?c.zip", "abc.zip", true},
		{"a?c.zip", "ac.zip", false},
		{"*", "anything", true},
		{"*发票*", "2024发票扫描.pdf", true},
		{"*发票*", "invoice.pdf", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"ZIP", "zip", false}, // matching is case sensitive
		// '*' is a legal byte in a file name. When both the pattern and the name
		// hold one, the wildcard branch must win; taking the literal branch made
		// "a*" fail to match "a*bc". Found by FuzzMatchName.
		{"a*", "a*bc", true},
		{"*", "*bc", true},
		{"**", "**", true},
		{"*.zip", "*.zip", true},
		{"?", "*", true},
	}
	for _, c := range cases {
		if got := MatchName(c.pattern, c.name); got != c.want {
			t.Errorf("MatchName(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

// TestDirPrefixIsSegmentAware is the trap that a plain strings.HasPrefix falls
// into: "keep/" must not match "keeper/".
func TestDirPrefixIsSegmentAware(t *testing.T) {
	cases := []struct {
		rel, prefix string
		want        bool
	}{
		{"keep/a.zip", "keep/", true},
		{"keep/deep/a.zip", "keep/", true},
		{"keeper/a.zip", "keep/", false},
		{"keep", "keep/", true}, // the directory itself is covered by its prefix
		{"a.zip", "keep/", false},
	}
	for _, c := range cases {
		if got := HasDirPrefix(c.rel, c.prefix); got != c.want {
			t.Errorf("HasDirPrefix(%q, %q) = %v, want %v", c.rel, c.prefix, got, c.want)
		}
	}
	if !UnderAnyPrefix([]string{"keep/", "tmp/"}, "tmp/x/y") {
		t.Error("UnderAnyPrefix missed a listed prefix")
	}
	if UnderAnyPrefix([]string{"keep/"}, "keeper/x") {
		t.Error("UnderAnyPrefix matched a prefix that is only a string prefix")
	}
}

func baseRule() *rules.Rule {
	return &rules.Rule{
		DSLVersion: 1,
		ScopeID:    "exports",
		Action:     "quarantine",
		Match: rules.Match{
			MinMTimeAgeSeconds:  int64((30 * 24 * time.Hour).Seconds()),
			BasenamePatterns:    []string{"*.zip"},
			RelativeDirPrefixes: []string{"logs/"},
		},
		Except: rules.Except{
			RelativeDirPrefixes: []string{"keep/"},
			BasenamePatterns:    []string{"*invoice*"},
		},
		RetentionSeconds: int64((30 * 24 * time.Hour).Seconds()),
	}
}

func TestDecideOrder(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour).UnixNano()
	r := baseRule()

	cases := []struct {
		name      string
		in        Input
		candidate bool
		reason    string
	}{
		{
			name:      "a matching old file is a candidate",
			in:        Input{Rel: "logs/a.zip", SizeBytes: 10, MTimeNS: old, NowNS: now.UnixNano()},
			candidate: true,
			reason:    ReasonCandidate,
		},
		{
			name:      "a protection always wins",
			in:        Input{Rel: "logs/a.zip", MTimeNS: old, NowNS: now.UnixNano(), Protected: true},
			candidate: false,
			reason:    ReasonProtected,
		},
		{
			name:      "an excluded directory wins over the match",
			in:        Input{Rel: "keep/a.zip", MTimeNS: old, NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonExceptDir,
		},
		{
			name:      "an excluded name wins over the match",
			in:        Input{Rel: "logs/a-invoice.zip", MTimeNS: old, NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonExceptName,
		},
		{
			name:      "a path outside the listed directories is kept",
			in:        Input{Rel: "other/a.zip", MTimeNS: old, NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonOutsideDir,
		},
		{
			name:      "a name that does not match is kept",
			in:        Input{Rel: "logs/a.txt", MTimeNS: old, NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonNameMismatch,
		},
		{
			name:      "a recent file is kept",
			in:        Input{Rel: "logs/a.zip", MTimeNS: now.Add(-time.Hour).UnixNano(), NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonTooNew,
		},
		{
			name:      "an mtime in the future is kept, not trusted",
			in:        Input{Rel: "logs/a.zip", MTimeNS: now.Add(48 * time.Hour).UnixNano(), NowNS: now.UnixNano()},
			candidate: false,
			reason:    ReasonMTimeFuture,
		},
	}
	for _, c := range cases {
		v := Decide(r, c.in)
		if v.Candidate != c.candidate || v.ReasonCode != c.reason {
			t.Errorf("%s: got candidate=%v reason=%s, want candidate=%v reason=%s",
				c.name, v.Candidate, v.ReasonCode, c.candidate, c.reason)
		}
	}
}

func TestAgeBoundaryIsInclusive(t *testing.T) {
	now := time.Now().UTC()
	r := baseRule()
	age := time.Duration(r.Match.MinMTimeAgeSeconds) * time.Second
	exact := now.Add(-age).UnixNano()

	if v := Decide(r, Input{Rel: "logs/a.zip", MTimeNS: exact, NowNS: now.UnixNano()}); !v.Candidate {
		t.Fatalf("a file exactly at the age boundary was not a candidate: %s", v.ReasonCode)
	}
	if v := Decide(r, Input{Rel: "logs/a.zip", MTimeNS: exact + 1, NowNS: now.UnixNano()}); v.Candidate {
		t.Fatalf("a file one nanosecond too young was a candidate")
	}
}

func TestSizeBounds(t *testing.T) {
	now := time.Now().UTC()
	max := int64(1000)
	r := baseRule()
	r.Match.BasenamePatterns = nil
	r.Match.RelativeDirPrefixes = nil
	r.Match.MinSizeBytes = 10
	r.Match.MaxSizeBytes = &max
	old := now.Add(-40 * 24 * time.Hour).UnixNano()

	if v := Decide(r, Input{Rel: "x.bin", SizeBytes: 9, MTimeNS: old, NowNS: now.UnixNano()}); v.ReasonCode != ReasonTooSmall {
		t.Errorf("size 9: %s", v.ReasonCode)
	}
	if v := Decide(r, Input{Rel: "x.bin", SizeBytes: 1001, MTimeNS: old, NowNS: now.UnixNano()}); v.ReasonCode != ReasonTooLarge {
		t.Errorf("size 1001: %s", v.ReasonCode)
	}
	if v := Decide(r, Input{Rel: "x.bin", SizeBytes: 500, MTimeNS: old, NowNS: now.UnixNano()}); !v.Candidate {
		t.Errorf("size 500 was rejected: %s", v.ReasonCode)
	}
}

// TestGlobalProtectionCoversTheDangerousNames checks the built-in list that
// exists so a bad rule cannot reach source control, an application bundle or a
// private key.
func TestGlobalProtectionCoversTheDangerousNames(t *testing.T) {
	for _, rel := range []string{
		".git/config",
		"repo/.git/objects/ab/cdef",
		"App.app/Contents/Info.plist",
		"Photo.photoslibrary/db",
		"state.db",
		"server.sqlite",
		"id_rsa",
		"cert.pem",
		"private.key",
		".env",
		".dustkeeper/objects/x",
	} {
		if ok, _ := GlobalProtected(rel); !ok {
			t.Errorf("%q is not globally protected", rel)
		}
	}
	// A plain data file stays reachable.
	for _, rel := range []string{"exports/a.zip", "logs/2024.txt"} {
		if ok, why := GlobalProtected(rel); ok {
			t.Errorf("%q was protected by %s", rel, why)
		}
	}
}

// TestGuardMatchesByIdentity is the APFS case-alias test: a protection recorded
// as "Keep/a.zip" must also cover "keep/a.zip" when they are the same object.
func TestGuardMatchesByIdentity(t *testing.T) {
	id := domain.FileIdentity{Device: 7, Inode: 42, Kind: "regular", VolumeID: "dev-7"}
	g := NewGuard([]ResolvedProtection{{
		RawRelative: []byte("Keep/a.zip"),
		MatchKind:   domain.MatchExact,
		Source:      domain.ProtectUser,
		Identity:    id,
	}})
	if ok, _ := g.CheckExact("Keep/a.zip", id); !ok {
		t.Fatal("the exact path did not match")
	}
	if ok, _ := g.CheckExact("keep/a.zip", id); !ok {
		t.Fatal("the same object under another spelling did not match")
	}
	other := domain.FileIdentity{Device: 7, Inode: 43, Kind: "regular", VolumeID: "dev-7"}
	if ok, _ := g.CheckExact("keep/a.zip", other); ok {
		t.Fatal("a different object matched a protection")
	}
}

func TestPolicyDigestIsStable(t *testing.T) {
	a := PolicyDigest()
	b := PolicyDigest()
	if a != b {
		t.Fatal("the policy digest changed between two calls")
	}
	if a == "" {
		t.Fatal("the policy digest is empty")
	}
}
