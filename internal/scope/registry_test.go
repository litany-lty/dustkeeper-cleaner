package scope

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/config"
	"dustkeeper/internal/domain"
	"dustkeeper/internal/errs"
	"dustkeeper/internal/store"
)

// newRegistry builds one isolated installation: state directory, database and a
// registrar that believes it is instance "inst_a".
func newRegistry(t *testing.T) (*Registrar, string) {
	t.Helper()
	base := t.TempDir()
	paths := config.Paths{
		ConfigDir:    filepath.Join(base, "config"),
		StateDir:     filepath.Join(base, "state"),
		DBFile:       filepath.Join(base, "state", "state.db"),
		ConfigFile:   filepath.Join(base, "config", "config.json"),
		InstanceLock: filepath.Join(base, "state", "instance.lock"),
		InstanceJSON: filepath.Join(base, "state", "instance.json"),
		ControlLock:  filepath.Join(base, "state", "control.lock"),
		PauseRequest: filepath.Join(base, "state", "pause.request"),
		LogDir:       filepath.Join(base, "state", "logs"),
	}
	for _, d := range []string{paths.ConfigDir, paths.StateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	st, err := store.Open(paths.DBFile)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := &Registrar{
		Store:      st,
		Paths:      paths,
		InstanceID: "inst_a",
		Now:        func() int64 { return 1_700_000_000_000_000_000 },
	}
	return reg, base
}

func makeTree(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, "trees", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tree: %v", err)
	}
	return dir
}

// TestRegisterCreatesAVaultOnlyForMutableKinds is the "registering is not
// authorising" rule expressed in the filesystem: a read-only scope gets no
// management directory at all, so nothing can be moved into it by accident.
func TestRegisterCreatesAVaultOnlyForMutableKinds(t *testing.T) {
	reg, base := newRegistry(t)

	for _, tc := range []struct {
		kind      string
		wantVault bool
	}{
		{"observe", false},
		{"reference", false},
		{"assisted", true},
		{"managed_disposable", true},
	} {
		dir := makeTree(t, base, tc.kind)
		sc, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: tc.kind, Name: tc.kind})
		if err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		if string(sc.ID) != tc.kind {
			t.Errorf("%s: scope id = %q", tc.kind, sc.ID)
		}

		vault := filepath.Join(dir, ".dustkeeper")
		_, statErr := os.Stat(vault)
		if tc.wantVault {
			if statErr != nil {
				t.Fatalf("%s: no vault was created: %v", tc.kind, statErr)
			}
			for _, sub := range []string{"objects", "manifests", "tmp", "owner.json"} {
				if _, err := os.Stat(filepath.Join(vault, sub)); err != nil {
					t.Errorf("%s: vault is missing %s: %v", tc.kind, sub, err)
				}
			}
			// The vault must not be world readable.
			fi, err := os.Stat(filepath.Join(vault, "objects"))
			if err != nil {
				t.Fatalf("%s: stat objects: %v", tc.kind, err)
			}
			if perm := fi.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("%s: vault permissions are too open: %o", tc.kind, perm)
			}
		} else if statErr == nil {
			t.Fatalf("%s: a read-only scope must not get a management directory", tc.kind)
		}
	}
}

func TestKindIsRequiredAndValidated(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "kind")

	for _, kind := range []string{"", "temporary", "Managed_Disposable", "delete"} {
		_, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: kind, Name: "x"})
		if err == nil {
			t.Fatalf("kind %q was accepted", kind)
		}
		if errs.CodeOf(err) != errs.CodeInvalidArgument {
			t.Errorf("kind %q: code = %s", kind, errs.CodeOf(err))
		}
	}
}

// TestDangerousPathsAreRefused covers the refusals that keep the tool away from
// the operating system and from its own state.
func TestDangerousPathsAreRefused(t *testing.T) {
	reg, base := newRegistry(t)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}

	cases := map[string]string{
		"the filesystem root": "/",
		"the home directory":  home,
		"the users directory": filepath.Dir(home),
		"a system directory":  "/usr",
	}
	for name, path := range cases {
		_, addErr := reg.Add(context.Background(), AddOptions{Path: path, Kind: "observe", Name: "bad"})
		if addErr == nil {
			t.Errorf("%s was accepted as a scope", name)
			continue
		}
		if code := errs.CodeOf(addErr); code != errs.CodeScopeRefused && code != errs.CodeInvalidArgument {
			t.Errorf("%s: code = %s (%v)", name, code, addErr)
		}
	}

	// A directory holding the state directory must be refused too: scanning it
	// would put the database itself in reach.
	_, err = reg.Add(context.Background(), AddOptions{Path: base, Kind: "observe", Name: "selfref"})
	if err == nil {
		t.Fatal("a path containing the state directory was accepted")
	}
	if errs.CodeOf(err) != errs.CodeScopeRefused {
		t.Errorf("code = %s, want SCOPE_REFUSED", errs.CodeOf(err))
	}
}

func TestMissingAndRelativePathsAreRejected(t *testing.T) {
	reg, base := newRegistry(t)
	_ = base

	if _, err := reg.Add(context.Background(), AddOptions{Path: "relative/dir", Kind: "observe", Name: "rel"}); err == nil {
		t.Fatal("a relative path was accepted")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := reg.Add(context.Background(), AddOptions{Path: missing, Kind: "observe", Name: "gone"}); err == nil {
		t.Fatal("a missing directory was accepted")
	}
	// A file is not a scope.
	file := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := reg.Add(context.Background(), AddOptions{Path: file, Kind: "observe", Name: "file"}); err == nil {
		t.Fatal("a regular file was accepted as a scope")
	}
}

func TestTheSameTreeCannotBeRegisteredTwice(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "twice")

	if _, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "observe", Name: "first"}); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "observe", Name: "second"})
	if err == nil {
		t.Fatal("the same directory was registered twice")
	}
	if code := errs.CodeOf(err); code != errs.CodeScopeOverlap && code != errs.CodeInvalidArgument {
		t.Errorf("code = %s, want SCOPE_OVERLAP", code)
	}
}

// TestNestedScopesAreRefused keeps the ownership question unambiguous: a tree
// inside another tree would be reachable by two scopes.
func TestNestedScopesAreRefused(t *testing.T) {
	reg, base := newRegistry(t)
	outer := makeTree(t, base, "outer")
	if _, err := reg.Add(context.Background(), AddOptions{Path: outer, Kind: "observe", Name: "outer"}); err != nil {
		t.Fatalf("outer: %v", err)
	}

	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir inner: %v", err)
	}
	_, err := reg.Add(context.Background(), AddOptions{Path: inner, Kind: "observe", Name: "inner"})
	if err == nil {
		t.Fatal("a directory nested inside another scope was accepted")
	}
	if code := errs.CodeOf(err); code != errs.CodeScopeOverlap && code != errs.CodeInvalidArgument {
		t.Errorf("code = %s, want SCOPE_OVERLAP", code)
	}
}

// TestOpenVerifiedDetectsAReplacedRoot is the check that keeps a stale plan from
// acting on a different directory: the root identity is compared, not the path.
func TestOpenVerifiedDetectsAReplacedRoot(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "replace")

	sc, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "observe", Name: "replace"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	vs, err := reg.OpenVerified(context.Background(), sc.ID)
	if err != nil {
		t.Fatalf("open verified: %v", err)
	}
	if vs.Root == nil {
		t.Fatal("OpenVerified returned no root handle")
	}
	vs.Root.Close()

	// Replace the directory with a different one at the same path.
	moved := dir + ".old"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("recreate: %v", err)
	}

	_, err = reg.OpenVerified(context.Background(), sc.ID)
	if err == nil {
		t.Fatal("a replaced root was accepted")
	}
	if code := errs.CodeOf(err); code != errs.CodeScopeChanged {
		t.Errorf("code = %s, want SCOPE_CHANGED", code)
	}
}

// TestOwnerMarkerBlocksAForeignInstance covers the two-state-directory case: an
// owner marker written by another instance must stop a mutable registration
// outright. The probe walks the whole subtree, so it only runs for kinds that
// can mutate — a read-only registration over a foreign tree is still refused by
// the overlap rule, not by the marker scan.
func TestOwnerMarkerBlocksAForeignInstance(t *testing.T) {
	reg, base := newRegistry(t)
	dir := makeTree(t, base, "owned")

	marker := filepath.Join(dir, ".dustkeeper")
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatalf("mkdir marker: %v", err)
	}
	body := `{"format_version":1,"instance_id":"inst_b","scope_id":"owned","kind":"managed_disposable"}`
	if err := os.WriteFile(filepath.Join(marker, "owner.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	_, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "managed_disposable", Name: "owned"})
	if err == nil {
		t.Fatal("a tree owned by another instance was registered as mutable")
	}
	if code := errs.CodeOf(err); code != errs.CodeScopeRefused && code != errs.CodeScopeOverlap &&
		code != errs.CodeInvalidArgument {
		t.Errorf("code = %s, want a refusal (%v)", code, err)
	}
	// Nothing may have been written into the foreign tree.
	if _, err := os.Stat(filepath.Join(marker, "objects")); err == nil {
		t.Error("a vault was created inside a tree owned by another instance")
	}
}

func TestScopeIDShapeIsEnforced(t *testing.T) {
	reg, base := newRegistry(t)

	// A name that cannot be a scope ID is derived from the directory name.
	dir := makeTree(t, base, "Mixed CASE dir")
	sc, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "observe"})
	if err != nil {
		t.Fatalf("add without a name: %v", err)
	}
	if !scopeIDPattern.MatchString(string(sc.ID)) {
		t.Errorf("scope id %q does not match the documented shape", sc.ID)
	}

	// An explicitly invalid name must be rejected, not repaired silently.
	for _, name := range []string{"Upper", "1abc", "has space", "waytoolongwaytoolongwaytoolongwaytoolong"} {
		dir := makeTree(t, base, "n"+strings.ReplaceAll(name, " ", "_"))
		if _, err := reg.Add(context.Background(), AddOptions{Path: dir, Kind: "observe", Name: name}); err == nil {
			t.Errorf("scope name %q was accepted", name)
		}
	}
}

// TestVerificationRejectsAnUnregisteredScope keeps the id space closed.
func TestVerificationRejectsAnUnregisteredScope(t *testing.T) {
	reg, _ := newRegistry(t)
	_, err := reg.OpenVerified(context.Background(), domain.ScopeID("nope"))
	if err == nil {
		t.Fatal("verification of an unregistered scope succeeded")
	}
	if errs.CodeOf(err) != errs.CodeScopeNotFound {
		t.Errorf("code = %s, want SCOPE_NOT_FOUND", errs.CodeOf(err))
	}
}
