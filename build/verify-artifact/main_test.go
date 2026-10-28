package main

import (
	"debug/elf"
	"encoding/binary"
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

// A dynamic section that cannot be parsed must fail the check.
//
// The previous version ignored the error from ImportedLibraries, so a file whose
// dynamic section was unreadable came out with "needed=0" and was labelled static —
// the one conclusion the policy must never reach by accident. A file with no
// dynamic section is a different fact: that is genuinely zero dependencies, and it
// is what a static artifact reports.
func TestAnUnreadableDynamicSectionIsAnErrorNotAnEmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-dynamic.elf")
	if err := os.WriteFile(path, elfWithBadDynamicSection(t), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	facts, err := inspect(path, "elf", "amd64")
	if err == nil {
		t.Fatalf("an unreadable dynamic section was accepted as %+v", facts)
	}
	if !strings.Contains(err.Error(), "dynamic section") {
		t.Fatalf("the failure does not name the dynamic section: %v", err)
	}
}

// elfWithBadDynamicSection builds the smallest ELF64 file that has a SHT_DYNAMIC
// section whose length is not a multiple of a dynamic entry, which the standard
// library reports as an error rather than as "no libraries".
func elfWithBadDynamicSection(t *testing.T) []byte {
	t.Helper()
	const (
		ehSize   = 64
		shSize   = 64
		dynOff   = 200
		dynBytes = 7 // deliberately not a multiple of 16
	)
	buf := make([]byte, dynOff+dynBytes)
	le := binary.LittleEndian

	ident := buf[:16]
	copy(ident, []byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT)})

	le.PutUint16(buf[16:], uint16(elf.ET_EXEC))
	le.PutUint16(buf[18:], uint16(elf.EM_X86_64))
	le.PutUint32(buf[20:], uint32(elf.EV_CURRENT))
	le.PutUint64(buf[40:], ehSize) // e_shoff
	le.PutUint16(buf[52:], ehSize) // e_ehsize
	le.PutUint16(buf[58:], shSize) // e_shentsize
	le.PutUint16(buf[60:], 2)      // e_shnum

	// Section header 1: the dynamic section.
	sh := buf[ehSize+shSize:]
	le.PutUint32(sh[0:], 0) // sh_name
	le.PutUint32(sh[4:], uint32(elf.SHT_DYNAMIC))
	le.PutUint64(sh[24:], dynOff)   // sh_offset
	le.PutUint64(sh[32:], dynBytes) // sh_size
	le.PutUint64(sh[48:], 8)        // sh_addralign
	le.PutUint64(sh[56:], 16)       // sh_entsize
	for i := dynOff; i < len(buf); i++ {
		buf[i] = 0xff
	}
	return buf
}
