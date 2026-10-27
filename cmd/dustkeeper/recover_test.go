package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"dustkeeper/internal/rescue"
)

// buildVault writes the smallest scope that still has a real manifest: one
// object, its manifest, and the owner marker. It is written by hand because the
// point of the rescue path is that it reads the directory, not the code that
// created it.
func buildVault(t *testing.T, root string) (objectID string, objectsDir string) {
	t.Helper()
	vault := filepath.Join(root, ".dustkeeper")
	objectsDir = filepath.Join(vault, "objects")
	manifests := filepath.Join(vault, "manifests")
	for _, d := range []string{objectsDir, manifests} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	owner := []byte(`{"instance_id":"inst_x","scope_id":"scp_x"}`)
	if err := os.WriteFile(filepath.Join(vault, "owner.json"), owner, 0o600); err != nil {
		t.Fatalf("write owner: %v", err)
	}

	objectID = "obj_00000000000000000000000000000000"
	manifest := map[string]any{
		"format_version":    1,
		"instance_id":       "inst_x",
		"scope_id":          "scp_x",
		"object_id":         objectID,
		"raw_relative_b64":  "b2xkLnppcA==", // "old.zip"
		"content_hash":      "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		"metadata_hash":     "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"plan_digest":       "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"created_at_ns":     1,
		"expected_identity": map[string]any{"inode": 1, "device": 1},
		"parent_identity":   map[string]any{"inode": 1, "device": 1},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifests, objectID+".json"), body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(objectsDir, objectID), []byte("payload"), 0o600); err != nil {
		t.Fatalf("write object: %v", err)
	}
	return objectID, objectsDir
}

// A23: through the real entry point, an unreadable objects directory must not be
// reported as "the object is gone", and the exit status must say the inventory is
// incomplete rather than healthy.
func TestRecoverInspectReportsAnUnreadableObjectsDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode bits do not deny access")
	}
	base := newHome(t)
	root := filepath.Join(base, "scope")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	objectID, objectsDir := buildVault(t, root)
	if err := os.Chmod(objectsDir, 0o000); err != nil {
		t.Fatalf("chmod objects: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(objectsDir, 0o700) })

	r := cli(t, "recover", "inspect", "--root", root, "--json")
	if r.code != 6 {
		t.Fatalf("exit = %d, want 6 (RECOVERY_REQUIRED); stdout:\n%s", r.code, r.stdout)
	}
	if got := r.str(t, "code"); got != "RECOVERY_REQUIRED" {
		t.Fatalf("code = %q, want RECOVERY_REQUIRED", got)
	}
	data, ok := r.env["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object: %v", r.env["data"])
	}
	if got := data["object_state"]; got != rescue.ObjectStateUnreadable {
		t.Fatalf("object_state = %v, want %q", got, rescue.ObjectStateUnreadable)
	}
	if got := data["manifest_without_object"]; got != float64(0) {
		t.Fatalf("manifest_without_object = %v; an unreadable directory proves no absence", got)
	}
	if got := data["unverified"]; got != float64(1) {
		t.Fatalf("unverified = %v, want 1", got)
	}
	entries, _ := data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	entry, _ := entries[0].(map[string]any)
	if got := entry["status"]; got != rescue.StatusUnverified {
		t.Fatalf("entry status = %v, want %q", got, rescue.StatusUnverified)
	}
	if got := entry["object"]; got != objectID {
		t.Fatalf("entry object = %v, want %s", got, objectID)
	}
	if got := entry["raw_relative"]; got != "old.zip" {
		t.Fatalf("entry raw_relative = %v, want old.zip", got)
	}
	// The operator is told what to do next instead of being left with a count.
	if len(r.env["next_steps"].([]any)) == 0 {
		t.Fatal("the envelope offers no next step for an unverifiable inventory")
	}
}

// A23: the same entry point on a healthy directory stays a success, so the
// RECOVERY_REQUIRED above is caused by the unreadable directory and not by the
// command always failing.
func TestRecoverInspectOnAReadableDirectorySucceeds(t *testing.T) {
	base := newHome(t)
	root := filepath.Join(base, "scope")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir scope: %v", err)
	}
	buildVault(t, root)

	r := cli(t, "recover", "inspect", "--root", root, "--json")
	if r.code != 0 {
		t.Fatalf("exit = %d, want 0; stdout:\n%s", r.code, r.stdout)
	}
	data, ok := r.env["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object: %v", r.env["data"])
	}
	if got := data["object_state"]; got != rescue.ObjectStateRead {
		t.Fatalf("object_state = %v, want %q", got, rescue.ObjectStateRead)
	}
	if got := data["complete"]; got != float64(1) {
		t.Fatalf("complete = %v, want 1", got)
	}
}
