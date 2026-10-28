// Command verify-artifact reads a built binary's headers and prints the facts the
// release manifest records.
//
// It exists because the release gate used to prove only that a file existed. A
// filename check ("CC contains musl") proves nothing about the artifact either, and
// `strings | grep musl` proves less: a glibc build can contain the word. The facts
// that decide whether a Linux artifact is the static musl build the policy claims
// are in its headers — a program interpreter entry and shared-library needs. If
// those are absent, the binary really is self-contained.
//
// Usage:
//
//	verify-artifact -file dist/dustkeeper-1.0.0-phase1-linux-amd64 \
//	                -format elf -machine amd64 [-require static]
//
// On success it prints one line of key=value facts and exits 0. On a contradiction
// (wrong container, wrong architecture, a dynamic dependency where a static binary
// was required, or headers it cannot read) it prints the reason to stderr and exits
// 3, so the caller cannot mistake "could not check" for "checked and fine".
package main

import (
	"debug/elf"
	"debug/macho"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	file := flag.String("file", "", "path to the built binary")
	format := flag.String("format", "", "elf or macho")
	machine := flag.String("machine", "", "expected architecture: amd64 or arm64")
	require := flag.String("require", "", "static (ELF only): refuse a binary that needs a loader or shared libraries")
	flag.Parse()

	if *file == "" || *format == "" || *machine == "" {
		fmt.Fprintln(os.Stderr, "verify-artifact: -file, -format and -machine are required")
		os.Exit(2)
	}

	facts, err := inspect(*file, *format, *machine)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-artifact: "+err.Error())
		os.Exit(3)
	}
	if *require == "static" && facts.linkage != "static" {
		fmt.Fprintf(os.Stderr,
			"verify-artifact: %s needs %s; a static build was required\n",
			*file, facts.detail())
		os.Exit(3)
	}
	fmt.Println(facts.String())
}

// artifactFacts is deliberately flat: the manifest is a list of key=value lines, and
// a nested structure here would only have to be flattened again.
type artifactFacts struct {
	format  string
	machine string
	linkage string
	interp  string
	needed  []string
}

func (f artifactFacts) detail() string {
	if f.interp != "" {
		return "program interpreter " + f.interp
	}
	if len(f.needed) > 0 {
		return "shared libraries " + strings.Join(f.needed, ",")
	}
	return "a dynamic loader"
}

func (f artifactFacts) String() string {
	interp := f.interp
	if interp == "" {
		interp = "none"
	}
	return fmt.Sprintf("format=%s machine=%s linkage=%s interp=%s needed=%d",
		f.format, f.machine, f.linkage, interp, len(f.needed))
}

func inspect(path, format, want string) (artifactFacts, error) {
	switch format {
	case "elf":
		return inspectELF(path, want)
	case "macho":
		return inspectMachO(path, want)
	}
	return artifactFacts{}, fmt.Errorf("unknown format %q", format)
}

// elfMachines maps the arch names this project builds onto ELF machine ids.
var elfMachines = map[string]elf.Machine{
	"amd64": elf.EM_X86_64,
	"arm64": elf.EM_AARCH64,
}

func inspectELF(path, want string) (artifactFacts, error) {
	f, err := elf.Open(path)
	if err != nil {
		return artifactFacts{}, fmt.Errorf("%s is not a readable ELF file: %w", path, err)
	}
	defer f.Close()

	wantMachine, ok := elfMachines[want]
	if !ok {
		return artifactFacts{}, fmt.Errorf("unknown architecture %q", want)
	}
	if f.Machine != wantMachine {
		return artifactFacts{}, fmt.Errorf("%s is %s, expected %s", path, f.Machine, wantMachine)
	}

	facts := artifactFacts{format: "elf", machine: want}
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return artifactFacts{}, fmt.Errorf("%s is %s, not an executable", path, f.Type)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			// Read the interpreter path so the failure names it.
			if raw, err := io.ReadAll(p.Open()); err == nil {
				facts.interp = strings.TrimRight(string(raw), "\x00")
			} else {
				facts.interp = "(unreadable)"
			}
		}
	}
	if libs, err := neededLibraries(f); err != nil {
		// A dynamic section that cannot be parsed is not "no dependencies". Treating
		// a parse failure as an empty list is how a binary with a loader would be
		// reported as static.
		return artifactFacts{}, fmt.Errorf("%s has a dynamic section that could not be read: %w", path, err)
	} else {
		facts.needed = libs
	}
	if facts.interp == "" && len(facts.needed) == 0 {
		facts.linkage = "static"
	} else {
		facts.linkage = "dynamic"
	}
	return facts, nil
}

// neededLibraries lists the shared libraries an ELF file requires.
//
// It distinguishes the two outcomes that used to be merged: a file with no dynamic
// section has no dependencies (nil, nil), while a dynamic section that cannot be
// read is an error the caller must fail on.
func neededLibraries(f *elf.File) ([]string, error) {
	return f.ImportedLibraries()
}

// machoMachines maps the arch names onto Mach-O cpu types. A darwin binary is never
// claimed to be static: it links libSystem by design, and saying otherwise would be
// the kind of claim this tool exists to prevent.
var machoMachines = map[string]macho.Cpu{
	"amd64": macho.CpuAmd64,
	"arm64": macho.CpuArm64,
}

func inspectMachO(path, want string) (artifactFacts, error) {
	f, err := macho.Open(path)
	if err != nil {
		return artifactFacts{}, fmt.Errorf("%s is not a readable Mach-O file: %w", path, err)
	}
	defer f.Close()

	wantCPU, ok := machoMachines[want]
	if !ok {
		return artifactFacts{}, fmt.Errorf("unknown architecture %q", want)
	}
	if f.Cpu != wantCPU {
		return artifactFacts{}, fmt.Errorf("%s is %s, expected %s", path, f.Cpu, wantCPU)
	}
	facts := artifactFacts{format: "macho", machine: want, linkage: "linked"}
	if libs, lerr := f.ImportedLibraries(); lerr != nil {
		return artifactFacts{}, fmt.Errorf("%s has load commands that could not be read: %w", path, lerr)
	} else {
		facts.needed = libs
	}
	return facts, nil
}
