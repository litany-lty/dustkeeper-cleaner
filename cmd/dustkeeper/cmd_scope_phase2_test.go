package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dustkeeper/internal/domain"
)

func TestScopePauseDoesNotOverwriteHeldSafetyState(t *testing.T) {
	base := newHome(t)
	mustCLI(t, "init", "--json")
	root := filepath.Join(base, "scope")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mustCLI(t, "scope", "add", "--path", root, "--kind", "observe", "--name", "exports", "--json")
	st := openState(t, base)
	if err := st.SetScopeStatus(t.Context(), "exports", domain.ScopeHeld); err != nil {
		t.Fatalf("hold scope: %v", err)
	}

	result := cli(t, "scope", "pause", "--id", "exports", "--json")
	if result.code == 0 {
		t.Fatalf("pause overwrote held safety state:\n%s", result.stdout)
	}
	if got := result.str(t, "code"); got != "HELD" {
		t.Fatalf("code=%q, want HELD; stdout:\n%s", got, result.stdout)
	}
	got, err := st.GetScope(t.Context(), "exports")
	if err != nil {
		t.Fatalf("get scope: %v", err)
	}
	if got.Status != domain.ScopeHeld {
		t.Fatalf("scope status=%q, want held", got.Status)
	}
}

func TestScopeListRejectsUnknownKind(t *testing.T) {
	newHome(t)
	mustCLI(t, "init", "--json")

	result := cli(t, "scope", "list", "--kind", "temporary", "--json")
	if result.code == 0 {
		t.Fatalf("unknown kind was treated as an empty result:\n%s", result.stdout)
	}
	if got := result.str(t, "code"); got != "INVALID_ARGUMENT" {
		t.Fatalf("code=%q, want INVALID_ARGUMENT", got)
	}
}

func TestScopePauseRejectsResumeOnlyFlag(t *testing.T) {
	base := newHome(t)
	mustCLI(t, "init", "--json")
	root := filepath.Join(base, "scope")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	mustCLI(t, "scope", "add", "--path", root, "--kind", "observe", "--name", "exports", "--json")

	result := cli(t, "scope", "pause", "--id", "exports", "--accept-stale-hold", "--json")
	if result.code == 0 {
		t.Fatalf("pause silently ignored a resume-only flag:\n%s", result.stdout)
	}
	if got := result.str(t, "code"); got != "INVALID_ARGUMENT" {
		t.Fatalf("code=%q, want INVALID_ARGUMENT", got)
	}
}

func TestScopeHumanOutputQuotesUntrustedRootPath(t *testing.T) {
	base := newHome(t)
	mustCLI(t, "init", "--json")
	root := filepath.Join(base, "scope\n\x1b[31mforged")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	commands := [][]string{
		{"scope", "add", "--path", root, "--kind", "observe", "--name", "exports"},
		{"scope", "list"},
		{"scope", "show", "--id", "exports"},
	}
	for _, args := range commands {
		result := cli(t, args...)
		if result.code != 0 {
			t.Fatalf("scope command %v failed: code=%d stdout=%s stderr=%s", args, result.code, result.stdout, result.stderr)
		}
		if strings.Contains(result.stdout, "\x1b") || strings.Contains(result.stdout, "scope\n") {
			t.Fatalf("scope root injected terminal controls:\n%s", result.stdout)
		}
		if !strings.Contains(result.stdout, `\n`) || !strings.Contains(result.stdout, `\x1b`) {
			t.Fatalf("scope root was not visibly escaped:\n%s", result.stdout)
		}
	}
}
