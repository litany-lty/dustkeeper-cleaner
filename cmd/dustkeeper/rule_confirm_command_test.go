package main

// C08 and the quoting note: the confirmation command a preview prints has to be
// runnable as it stands.
//
// `rule authorize` still exists as the single-capability entry point, and its
// proposal used to print a line without --max-files/--max-bytes. Copying that line
// recomputed the digest from the configured default and the commit came back
// DIGEST_MISMATCH - so the approved ceiling could not be committed at all through
// this path. The grant is what an unattended run is allowed to do, so "the printed
// command is the approval" is not cosmetic.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThePrintedAuthorizeCommandCarriesTheApprovedCeilings(t *testing.T) {
	base, _ := enableTree(t, "managed_disposable")
	// Deliberately not the approved numbers: a command that inherits the
	// configuration instead of the screen would commit these.
	setRuntime(t, base, map[string]any{
		"max_auto_quarantine_files": 100,
		"max_auto_quarantine_bytes": 1 << 30,
	})

	// The rule has to be active before a capability can be granted for it.
	prev := mustCLI(t, "rule", "preview", "--id", "r1", "--revision", "1", "--json")
	mustCLI(t, "rule", "activate", "--id", "r1", "--revision", "1",
		"--digest", dataString(t, prev, "review_digest"), "--json")

	proposal := mustCLI(t, "rule", "authorize", "--id", "r1", "--revision", "1",
		"--auto-quarantine", "--max-files", "3", "--max-bytes", "4096", "--json")
	line := nextSteps(t, proposal)[0]
	for _, want := range []string{"--max-files 3", "--max-bytes 4096"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the printed confirmation does not carry %q: %s", want, line)
		}
	}

	confirmed := runPrintedCommand(t, line)
	if got, _ := dataOf(t, confirmed)["max_files"].(float64); got != 3 {
		t.Fatalf("the printed confirmation committed %v files, not 3", got)
	}
	if got, _ := dataOf(t, confirmed)["max_bytes"].(float64); got != 4096 {
		t.Fatalf("the printed confirmation committed %v bytes, not 4096", got)
	}
	if got := dataString(t, confirmed, "digest"); got != dataString(t, proposal, "approval_digest") {
		t.Fatalf("the confirmed digest %s is not the approved %s", got, dataString(t, proposal, "approval_digest"))
	}
}

// A rule ID is any non-empty string, so the printed command has to survive one
// that the shell would otherwise split.
func TestThePrintedCommandsQuoteARuleIDThatNeedsIt(t *testing.T) {
	base, _ := enableTree(t, "managed_disposable")

	rulePath := filepath.Join(base, "rule.json")
	body, err := os.ReadFile(rulePath)
	if err != nil {
		t.Fatalf("read the rule: %v", err)
	}
	spaced := filepath.Join(base, "rule-spaced.json")
	if err := os.WriteFile(spaced, body, 0o600); err != nil {
		t.Fatalf("write the rule: %v", err)
	}
	const id = "daily exports"
	mustCLI(t, "rule", "import", "--file", spaced, "--id", id, "--json")

	preview := mustCLI(t, "rule", "enable", "--id", id, "--revision", "1", "--json")
	line := nextSteps(t, preview)[0]
	if !strings.Contains(line, "--id 'daily exports'") {
		t.Fatalf("the printed command does not quote the rule ID: %s", line)
	}
	// And the quoted form is what the ID lookup needs: the same ID with the quote
	// stripped is a different rule.
	if strings.Contains(line, "--id daily exports") {
		t.Fatalf("the printed command is still split by the shell: %s", line)
	}
}

// shellQuote is the rule behind both lines: simple identifiers unchanged,
// everything else in single quotes with embedded quotes escaped.
func TestShellQuoteLeavesSimpleWordsAndQuotesTheRest(t *testing.T) {
	cases := []struct{ in, want string }{
		{"r1", "r1"},
		{"rule-001", "rule-001"},
		{"daily exports", "'daily exports'"},
		{"it's", `'it'\''s'`},
		{"", "''"},
		{"a;b", "'a;b'"},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
