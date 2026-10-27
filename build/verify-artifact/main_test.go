package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildPureGoLinux produces a real ELF for the verifier to read.
//
// CGO is off on purpose: the point of the fixture is the container and the linkage,
// not the database driver, and a pure-Go linux binary is a genuine static ELF with no
// interpreter — exactly what the release policy claims about its own artifacts.
func buildPureGoLinux(t *testing.T, arch string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "fixture-linux-"+arch)
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(),
		"GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0",
		"GOFLAGS=-buildvcs=false",
	)
	// The package under ./ is this command, which builds a real binary.
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot cross-compile a fixture here: %v\n%s", err, msg)
	}
	return out
}

func TestAPureGoLinuxBinaryIsRecognisedAsStatic(t *testing.T) {
	path := buildPureGoLinux(t, "amd64")

	facts, err := inspect(path, "elf", "amd64")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if facts.format != "elf" || facts.machine != "amd64" {
		t.Fatalf("facts: %+v", facts)
	}
	if facts.linkage != "static" || facts.interp != "" || len(facts.needed) != 0 {
		t.Fatalf("a pure-Go linux binary should be static, got %+v", facts)
	}
}

// The architecture has to be checked, not assumed from the filename. A build that
// silently produced the wrong architecture would otherwise be published as correct.
func TestTheWrongArchitectureIsRefused(t *testing.T) {
	path := buildPureGoLinux(t, "amd64")

	if _, err := inspect(path, "elf", "arm64"); err == nil {
		t.Fatal("an amd64 binary was accepted as arm64")
	} else if !strings.Contains(err.Error(), "expected") {
		t.Fatalf("the refusal should name the expectation: %v", err)
	}
}

// A file that is not the container the target claims must not be inspected as one.
func TestATextFileIsNotAnArtifact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("stub binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect(path, "elf", "amd64"); err == nil {
		t.Fatal("a text file was accepted as an ELF artifact")
	}
}

// The darwin path is exercised against a real Mach-O: this test binary.
func TestTheDarwinPathReadsARealMachO(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the Mach-O fixture is the test binary, which is only Mach-O on darwin")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	facts, err := inspect(self, "macho", runtime.GOARCH)
	if err != nil {
		t.Fatalf("inspect %s: %v", self, err)
	}
	if facts.format != "macho" || facts.machine != runtime.GOARCH {
		t.Fatalf("facts: %+v", facts)
	}
	// A darwin binary links libSystem; claiming "static" here would be a lie.
	if facts.linkage != "linked" {
		t.Fatalf("darwin linkage should be reported as linked, got %q", facts.linkage)
	}
}
