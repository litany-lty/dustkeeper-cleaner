package policy

import (
	"strings"

	"dustkeeper/internal/domain"
)

// PolicyVersion is bumped whenever the built-in protection list changes. The
// version enters every plan binding, so an updated list invalidates plans made
// under the old one.
const PolicyVersion = 1

// globalRule is one built-in protection.
type globalRule struct {
	kind   string // segment | suffix | name
	value  string
	reason string
}

// The list is deliberately short and explicable. It is not a classifier for
// "important files": automatic action still depends on the scope contract, and
// this list only refuses the categories where a mistake is unrecoverable.
var globalRules = []globalRule{
	{"segment", ".git", "source repository metadata"},
	{"segment", ".hg", "source repository metadata"},
	{"segment", ".svn", "source repository metadata"},
	{"segment", ".dustkeeper", "Dustkeeper private directory"},

	{"suffix", ".app", "application bundle"},
	{"suffix", ".bundle", "application bundle"},
	{"suffix", ".framework", "application framework"},
	{"suffix", ".xcodeproj", "project bundle"},
	{"suffix", ".xcworkspace", "project bundle"},
	{"suffix", ".playground", "project bundle"},
	{"suffix", ".photoslibrary", "photo library"},
	{"suffix", ".photolibrary", "photo library"},
	{"suffix", ".imovielibrary", "media library"},
	{"suffix", ".tvlibrary", "media library"},

	{"suffix", ".db", "database file"},
	{"suffix", ".sqlite", "database file"},
	{"suffix", ".sqlite3", "database file"},
	{"suffix", ".db-wal", "database write-ahead log"},
	{"suffix", ".db-shm", "database shared memory file"},
	{"suffix", ".mdb", "database file"},
	{"suffix", ".accdb", "database file"},

	{"suffix", ".key", "key material"},
	{"suffix", ".pem", "key material"},
	{"suffix", ".crt", "certificate"},
	{"suffix", ".cer", "certificate"},
	{"suffix", ".p12", "key material"},
	{"suffix", ".pfx", "key material"},
	{"suffix", ".kdbx", "password database"},
	{"suffix", ".gpg", "encrypted material"},
	{"suffix", ".asc", "signature material"},

	{"name", ".env", "environment configuration"},
	{"name", "id_rsa", "private key"},
	{"name", "id_ed25519", "private key"},
	{"name", "id_ecdsa", "private key"},
	{"name", ".netrc", "credential file"},
	{"name", ".pgpass", "credential file"},
	{"name", "credentials", "credential file"},

	{"suffix", ".bak", "backup copy"},
	{"suffix", ".backup", "backup copy"},
	{"suffix", ".log", "active log file"},
}

// GlobalProtected reports whether a relative path is protected by the built-in
// list, with a human reason.
func GlobalProtected(rel string) (bool, string) {
	if rel == "" {
		return true, "scope root is not a file"
	}
	segments := strings.Split(rel, "/")
	name := segments[len(segments)-1]

	for _, r := range globalRules {
		switch r.kind {
		case "segment":
			for _, s := range segments[:len(segments)-1] {
				if s == r.value {
					return true, r.reason
				}
			}
		case "suffix":
			// The suffix is checked against the file name and against every
			// directory segment. A rule that matches "*.plist" must not reach
			// inside "App.app/", and a rule for "*" must not empty a photo
			// library. Over-protecting a directory that happens to end in one of
			// these suffixes only keeps files; under-protecting loses them.
			if strings.HasSuffix(name, r.value) {
				return true, r.reason
			}
			for _, seg := range segments[:len(segments)-1] {
				if strings.HasSuffix(seg, r.value) {
					return true, r.reason
				}
			}
		case "name":
			if name == r.value {
				return true, r.reason
			}
		}
	}
	return false, ""
}

// PolicyDigest summarises the protection policy. It is part of every plan
// binding so that a changed list cannot silently reuse an old approval.
func PolicyDigest() domain.Digest {
	var b strings.Builder
	b.WriteString("v")
	b.WriteByte(byte('0' + PolicyVersion))
	for _, r := range globalRules {
		b.WriteString("|")
		b.WriteString(r.kind)
		b.WriteString(":")
		b.WriteString(r.value)
	}
	return domain.DigestOf("dustkeeper/policy/v1", []byte(b.String()))
}

// ResolvedProtection is a protection together with the identity the kernel
// reported for it at scan start. A zero identity means it could not be resolved
// right now, which is not the same as "no protection".
type ResolvedProtection struct {
	RawRelative []byte
	MatchKind   string
	Source      string
	Identity    domain.FileIdentity
}

// Guard answers protection questions by identity, not by string comparison.
//
// This matters on APFS, where "Case.TXT" and "case.txt" are one object. A
// string comparison would let a differently-cased path walk straight past a
// protection that was recorded under the original spelling.
type Guard struct {
	exactPath map[string]string
	exactID   map[string]string
	subtree   []ResolvedProtection
	subtreeID map[string]string
}

// IdentityKey renders a stable key for an identity.
func IdentityKey(id domain.FileIdentity) string {
	if id.Inode == 0 && id.Device == 0 {
		return ""
	}
	return id.VolumeID + "|" + itoa(id.Device) + "|" + itoaU(id.Inode)
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func itoaU(v uint64) string { return itoa(v) }

// NewGuard indexes protections for fast lookup.
func NewGuard(ps []ResolvedProtection) *Guard {
	g := &Guard{
		exactPath: make(map[string]string, len(ps)),
		exactID:   make(map[string]string, len(ps)),
		subtreeID: make(map[string]string, len(ps)),
	}
	for _, p := range ps {
		rel := string(p.RawRelative)
		switch p.MatchKind {
		case domain.MatchExact:
			g.exactPath[rel] = p.Source
			if key := IdentityKey(p.Identity); key != "" {
				g.exactID[key] = p.Source
			}
		case domain.MatchSubtree:
			g.subtree = append(g.subtree, p)
			if key := IdentityKey(p.Identity); key != "" {
				g.subtreeID[key] = p.Source
			}
		}
	}
	return g
}

// Empty reports whether the guard has no protections at all.
func (g *Guard) Empty() bool {
	return g == nil || (len(g.exactPath) == 0 && len(g.subtree) == 0)
}

// CheckExact reports whether this exact object is protected. Identity is
// checked first and the literal path second, so a path whose object was
// replaced still matches its recorded spelling.
func (g *Guard) CheckExact(rel string, id domain.FileIdentity) (bool, string) {
	if g == nil {
		return false, ""
	}
	if key := IdentityKey(id); key != "" {
		if src, ok := g.exactID[key]; ok {
			return true, "protected by " + src + " (same object)"
		}
	}
	if src, ok := g.exactPath[rel]; ok {
		return true, "protected by " + src
	}
	return false, ""
}

// CheckSubtree reports whether a directory object is a protected subtree root.
// Callers keep a stack of matches so every descendant is protected too.
func (g *Guard) CheckSubtree(rel string, dirID domain.FileIdentity) (bool, string) {
	if g == nil {
		return false, ""
	}
	for _, p := range g.subtree {
		if string(p.RawRelative) == rel {
			return true, "protected subtree by " + p.Source
		}
	}
	if key := IdentityKey(dirID); key != "" {
		if src, ok := g.subtreeID[key]; ok {
			return true, "protected subtree by " + src + " (same object)"
		}
	}
	return false, ""
}

// SubtreePaths returns the recorded subtree roots, used to detect an ancestor
// that was re-created under a different inode.
func (g *Guard) SubtreePaths() []string {
	if g == nil {
		return nil
	}
	out := make([]string, 0, len(g.subtree))
	for _, p := range g.subtree {
		out = append(out, string(p.RawRelative))
	}
	return out
}

// CheckFile is the single question every entry point must ask.
//
// Planning, quarantining, restoring and purging all decide "may this object be
// touched", and they used to ask it differently: the planner and the executor
// called CheckExact only, so a subtree protection added after a scan had no
// effect on a plan that was created afterwards. A plan is not stale in that
// case — it is new — so the protection-revision check could not catch it either.
//
// Exact match is answered by identity first; subtree match is answered by path
// containment, by the identity of the directories above the object, or both. The
// ancestry argument is what makes the answer independent of spelling: on a
// case-insensitive volume "Folder" and "folder" are one directory, and only the
// kernel's identity says so. Callers that have no descriptors pass none, and the
// path answer still applies.
func (g *Guard) CheckFile(rel string, id domain.FileIdentity, ancestors ...domain.FileIdentity) (bool, string) {
	if g == nil {
		return false, ""
	}
	if ok, why := g.CheckExact(rel, id); ok {
		return true, why
	}
	for _, p := range g.subtree {
		root := strings.TrimSuffix(string(p.RawRelative), "/")
		if root == "" {
			continue
		}
		if HasDirPrefix(rel, root) {
			return true, "subtree protected by " + p.Source
		}
	}
	if ok, why := g.CheckAncestors(ancestors); ok {
		return true, why
	}
	return false, ""
}

// CheckAncestors reports whether any of these directories is the protected
// subtree root itself. The caller supplies them nearest-first, read through
// verified descriptors.
func (g *Guard) CheckAncestors(ancestors []domain.FileIdentity) (bool, string) {
	if g == nil || len(g.subtreeID) == 0 {
		return false, ""
	}
	for _, a := range ancestors {
		if key := IdentityKey(a); key != "" {
			if src, ok := g.subtreeID[key]; ok {
				return true, "subtree protected by " + src + " (an ancestor is the protected directory)"
			}
		}
	}
	return false, ""
}

// CheckNameOnly answers the same question without an identity, for callers that
// have not opened the object (a purge whose original file is already gone).
func (g *Guard) CheckNameOnly(rel string) (bool, string) {
	if g == nil {
		return false, ""
	}
	if src, ok := g.exactPath[rel]; ok {
		return true, "protected by " + src
	}
	for _, p := range g.subtree {
		root := strings.TrimSuffix(string(p.RawRelative), "/")
		if root == "" {
			continue
		}
		if HasDirPrefix(rel, root) {
			return true, "subtree protected by " + p.Source
		}
	}
	return false, ""
}
