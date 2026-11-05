package main

import (
	"testing"

	"dustkeeper/internal/errs"
)

func TestA14RuleEnableRejectsNegativeExplicitCeilings(t *testing.T) {
	enableTree(t, "managed_disposable")
	for _, args := range [][]string{
		{"--max-files", "-1"},
		{"--max-bytes", "-1"},
		{"--mode", "cleanup", "--purge-max-files", "-1"},
		{"--mode", "cleanup", "--purge-max-bytes", "-1"},
	} {
		full := []string{"rule", "enable", "--id", "r1", "--revision", "1", "--json"}
		full = append(full, args...)
		got := cli(t, full...)
		if got.str(t, "code") != string(errs.CodeInvalidArgument) {
			t.Fatalf("negative ceiling %v returned code %s\n%s", args, got.str(t, "code"), got.stdout)
		}
	}
}

func TestA15RuleAuthorityCommandsRejectNegativeRevision(t *testing.T) {
	enableTree(t, "managed_disposable")
	commands := [][]string{
		{"rule", "enable", "--id", "r1", "--revision", "-1", "--json"},
		{"rule", "activate", "--id", "r1", "--revision", "-1", "--digest", "sha256:x", "--json"},
		{"rule", "authorize", "--id", "r1", "--revision", "-1", "--auto-quarantine", "--json"},
		{"rule", "revoke", "--id", "r1", "--revision", "-1", "--auto-quarantine", "--json"},
	}
	for _, command := range commands {
		got := cli(t, command...)
		if got.str(t, "code") != string(errs.CodeInvalidArgument) {
			t.Fatalf("negative revision for %v returned code %s\n%s", command[:2], got.str(t, "code"), got.stdout)
		}
	}
}

func TestA16LegacyAuthorizeCannotGrantAutomaticQuarantineToAssistedScope(t *testing.T) {
	enableTree(t, "assisted")
	preview := mustCLI(t, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
	mustCLI(t, "rule", "activate", "--id", "r1", "--revision", "1", "--digest", dataString(t, preview, "review_digest"), "--json")
	proposal := cli(t, "rule", "authorize", "--id", "r1", "--revision", "1", "--auto-quarantine", "--json")
	if proposal.str(t, "code") != string(errs.CodeGrantRequired) {
		t.Fatalf("assisted scope received an automatic quarantine proposal: code=%s\n%s", proposal.str(t, "code"), proposal.stdout)
	}
}
