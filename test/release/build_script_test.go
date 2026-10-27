// Package release exercises build/build.sh as a reviewer would: by running it.
//
// The script is the only thing standing between the source tree and a set of
// published binaries, so "it exits 0" has to mean "every declared target exists,
// is signed where the platform needs it, and is covered by a checksum that
// verifies". These tests drive the script with a stub toolchain so that a release
// gate can be tested without actually cross-compiling anything.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptDir is the temporary copy of the real build script laid out the way the
// script expects: <root>/build/build.sh writing into <root>/dist.
type scriptDir struct {
	root string
	out  string
	bin  string
}

const stubGo = `#!/bin/sh
# Stub toolchain: enough of "go env" and "go build" to exercise the release gate.
if [ "${1:-}" = "env" ]; then
	case "${2:-}" in
	GOOS) echo "$STUB_HOST_OS" ;;
	GOARCH) echo "$STUB_HOST_ARCH" ;;
	*) echo "" ;;
	esac
	exit 0
fi
if [ "${1:-}" = "build" ]; then
	shift
	out=""
	prev=""
	for a in "$@"; do
		if [ "$prev" = "-o" ]; then out="$a"; fi
		prev="$a"
	done
	if [ -z "$out" ]; then
		echo "stub go: build without -o" >&2
		exit 2
	fi
	if [ "${STUB_SKIP_ARTIFACT:-0}" = "1" ]; then
		exit 0
	fi
	# An artifact that runs, so the smoke step has something to execute.
	{
		printf '#!/bin/sh\n'
		printf 'echo "dustkeeper stub %s"\n' "${GOOS:-}-${GOARCH:-}"
	} >"$out"
	chmod 755 "$out"
	exit 0
fi
echo "stub go: unsupported invocation: $*" >&2
exit 2
`

// stubCC answers the two questions the libc policy asks a compiler: which triple it
// targets, and which version it is.
const stubCC = `#!/bin/sh
case "${1:-}" in
-dumpmachine) echo "${STUB_CC_TRIPLE:-x86_64-linux-musl}" ;;
--version) echo "stub cc 1.0, musl-flavoured for tests" ;;
*) exit 0 ;;
esac
`

// stubVerify stands in for build/verify-artifact. The script's own tests are about
// the gate's control flow, not about ELF parsing (that lives in
// build/verify-artifact's tests), so the facts it reports are configurable.
const stubVerify = `#!/bin/sh
if [ "${STUB_VERIFY_FAIL:-0}" = "1" ]; then
	echo "stub verify: the artifact needs a program interpreter" >&2
	exit 3
fi
echo "format=${STUB_VERIFY_FORMAT:-elf} machine=amd64 linkage=${STUB_VERIFY_LINKAGE:-static} interp=none needed=0"
`

const stubCodesign = `#!/bin/sh
if [ "${STUB_CODESIGN_FAIL:-0}" = "1" ]; then
	echo "stub codesign: refusing to sign" >&2
	exit 1
fi
exit 0
`

const stubShasum = `#!/bin/sh
if [ "${STUB_SHASUM_FAIL:-0}" = "1" ]; then
	echo "stub shasum: refusing to hash" >&2
	exit 1
fi
for c in /usr/bin/shasum /usr/bin/sha256sum /bin/sha256sum; do
	if [ -x "$c" ]; then exec "$c" "$@"; fi
done
echo "stub shasum: no real implementation available" >&2
exit 127
`

// newScriptDir copies the real build script into a throwaway tree and installs
// stub tools ahead of the real ones.
func newScriptDir(t *testing.T) *scriptDir {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	real := filepath.Join(filepath.Dir(file), "..", "..", "build", "build.sh")
	body, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("read build script: %v", err)
	}

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "build", "build.sh"), string(body), 0o755)
	mustWrite(t, filepath.Join(root, ".gitkeep"), "", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatalf("mkdir dist: %v", err)
	}

	bin := filepath.Join(root, "stub-bin")
	mustWrite(t, filepath.Join(bin, "go"), stubGo, 0o755)
	mustWrite(t, filepath.Join(bin, "codesign"), stubCodesign, 0o755)
	mustWrite(t, filepath.Join(bin, "shasum"), stubShasum, 0o755)
	mustWrite(t, filepath.Join(bin, "stub-cc"), stubCC, 0o755)
	mustWrite(t, filepath.Join(bin, "stub-verify"), stubVerify, 0o755)

	return &scriptDir{root: root, out: filepath.Join(root, "dist"), bin: bin}
}

func mustWrite(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// run executes the script with a hermetic environment: the stub tools first on
// PATH, no inherited cross-compiler variables, and a stubbed host platform.
func (s *scriptDir) run(t *testing.T, extra map[string]string, args ...string) (string, int) {
	t.Helper()

	env := []string{
		"PATH=" + s.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + s.root,
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"STUB_HOST_OS=darwin",
		"STUB_HOST_ARCH=arm64",
		// A prebuilt verifier: the script would otherwise compile
		// ./build/verify-artifact with the stub "go", which cannot compile anything.
		"VERIFY_ARTIFACT=" + filepath.Join(s.bin, "stub-verify"),
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}

	argv := append([]string{filepath.Join(s.root, "build", "build.sh")}, args...)
	cmd := exec.Command("/bin/sh", argv...)
	cmd.Dir = s.root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if ok := asExitError(err, &exit); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run build script: %v", err)
		}
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// crossCompilers is the environment a maintainer would have on a macOS host when
// every declared target is buildable, with a musl-targeting compiler for Linux.
func crossCompilers() map[string]string {
	return map[string]string{
		"CC_darwin_amd64": "stub-cc",
		"CC_linux_amd64":  "stub-cc",
		"CC_linux_arm64":  "stub-cc",
	}
}

func (s *scriptDir) artifacts(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(s.out)
	if err != nil {
		t.Fatalf("read dist: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func (s *scriptDir) sums(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.out, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read SHA256SUMS: %v", err)
	}
	return string(body)
}

// A release that silently skips three of its four targets is the failure this
// gate exists for: the published set looks complete because nobody compared it
// against a declared list.
func TestReleaseFailsWhenACrossTargetHasNoCompiler(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, nil, "--release")
	if code == 0 {
		t.Fatalf("a release with no cross compilers exited 0:\n%s", out)
	}
	for _, target := range []string{"darwin-amd64", "linux-amd64", "linux-arm64"} {
		if !strings.Contains(out, target) {
			t.Errorf("the failure does not name the missing target %s:\n%s", target, out)
		}
	}
	if !strings.Contains(out, "CC_darwin_amd64") {
		t.Errorf("the failure does not say which variable to set:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Errorf("an incomplete release still wrote SHA256SUMS")
	}
}

// Development builds are allowed to skip. The point is that skipping is reported
// and that a release does not behave this way.
func TestDevelopmentBuildMaySkipTargets(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, nil)
	if code != 0 {
		t.Fatalf("a development build failed: %s\n%s", out, out)
	}
	if !strings.Contains(out, "linux-amd64") || !strings.Contains(out, "not built") {
		t.Errorf("a skipped target was not reported:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "dustkeeper-1.0.0-phase1-darwin-arm64")); err != nil {
		t.Errorf("the native target was not built: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(s.sums(t)), "\n") + 1; lines != 1 {
		t.Errorf("SHA256SUMS covers %d artifacts, want the 1 that was built:\n%s", lines, s.sums(t))
	}
}

// The happy path has to hold too, otherwise the gate is just "always fail".
func TestReleaseSucceedsWhenEverythingIsPresent(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, crossCompilers(), "--release")
	if code != 0 {
		t.Fatalf("a complete release exited %d:\n%s", code, out)
	}
	for _, target := range []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64"} {
		path := filepath.Join(s.out, "dustkeeper-1.0.0-phase1-"+target)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s is missing: %v", target, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", target)
		}
		if !strings.Contains(s.sums(t), "dustkeeper-1.0.0-phase1-"+target) {
			t.Fatalf("SHA256SUMS omits %s:\n%s", target, s.sums(t))
		}
	}
	if lines := strings.Count(strings.TrimSpace(s.sums(t)), "\n") + 1; lines != 4 {
		t.Errorf("SHA256SUMS covers %d artifacts, want 4:\n%s", lines, s.sums(t))
	}

	manifest, err := os.ReadFile(filepath.Join(s.out, "release-manifest.txt"))
	if err != nil {
		t.Fatalf("read release manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "linux_libc=musl-static") {
		t.Errorf("the release manifest does not record the libc policy:\n%s", manifest)
	}
	for _, want := range []string{
		"linux_libc_proof=",
		"target=linux-amd64",
		"target=linux-arm64",
		"toolchain=x86_64-linux-musl",
		"compiler_version=",
		"smoke=ok",
	} {
		if !strings.Contains(string(manifest), want) {
			t.Errorf("the release manifest has no %q:\n%s", want, manifest)
		}
	}
}

// The libc policy cannot be proved by "-static": that flag asks the linker for a
// static link and says nothing about which libc was used. A glibc cross compiler
// would produce a libc-static binary that this script would label musl-static, so
// the compiler's own target triple is checked before anything is labelled.
func TestReleaseFailsWhenTheLinuxToolchainIsNotMusl(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release built with a glibc cross compiler exited 0:\n%s", out)
	}
	if !strings.Contains(out, "not musl") || !strings.Contains(out, "linux-amd64") {
		t.Errorf("the failure does not name the target and the reason:\n%s", out)
	}
	if !strings.Contains(out, "x86_64-linux-gnu") {
		t.Errorf("the failure does not quote the compiler's own triple:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Errorf("a release without a libc proof still wrote a manifest")
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Errorf("a release without a libc proof still wrote SHA256SUMS")
	}
}

// A development build may skip, but it must not quietly checksum an artifact whose
// policy could not be proved: that is how an unlabelled glibc binary reaches a user.
func TestDevelopmentBuildRefusesToChecksumAnUnprovedArtifact(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	out, code := s.run(t, env)
	if code != 0 {
		t.Fatalf("a development build should report the problem, not fail: %s\n%s", out, out)
	}
	if !strings.Contains(out, "not musl") {
		t.Errorf("the development build said nothing about the libc policy:\n%s", out)
	}
	if strings.Contains(s.sums(t), "linux-amd64") {
		t.Errorf("an artifact built against glibc was checksummed:\n%s", s.sums(t))
	}
}

// A developer on a glibc machine still needs a binary. The escape hatch exists so
// that the alternative is not "edit the script": the artifact is built, but labelled
// unproved, and a release refuses the same environment.
func TestADeveloperCanBuildAnUnprovedLinuxArtifactExplicitly(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["ALLOW_UNPROVED_LIBC"] = "1"

	out, code := s.run(t, env)
	if code != 0 {
		t.Fatalf("an explicit unproved development build failed:\n%s", out)
	}
	if !strings.Contains(out, "linkage is unproved") {
		t.Errorf("the build did not say the linkage is unproved:\n%s", out)
	}

	facts, err := os.ReadFile(filepath.Join(s.out, "build-facts.txt"))
	if err != nil {
		t.Fatalf("read facts: %v", err)
	}
	if !strings.Contains(string(facts), "target=linux-amd64 format=elf arch=amd64 linkage=unproved") {
		t.Errorf("the facts do not record the unproved linkage:\n%s", facts)
	}
	if strings.Contains(string(facts), "linkage=static toolchain=x86_64-linux-gnu") {
		t.Errorf("a glibc build was recorded as a static musl artifact:\n%s", facts)
	}

	// The same environment cannot produce a release, whatever the variable says.
	if out, code := s.run(t, env, "--release"); code == 0 {
		t.Fatalf("ALLOW_UNPROVED_LIBC produced a release:\n%s", out)
	}
}

// The artifact itself has to be read, not inferred from the compiler. A binary with
// a program interpreter is not the static build the policy promises, whatever the
// compiler says it targets.
func TestReleaseFailsWhenTheArtifactIsNotStatic(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_VERIFY_FAIL"] = "1"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release with a dynamically linked artifact exited 0:\n%s", out)
	}
	if !strings.Contains(out, "not a static") {
		t.Errorf("the failure does not say the artifact is not static:\n%s", out)
	}
}

// Signing failure used to be `|| true`, so a macOS release could publish an
// unsigned binary and still exit 0.
func TestReleaseFailsWhenSigningFails(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_CODESIGN_FAIL"] = "1"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release with unsigned macOS binaries exited 0:\n%s", out)
	}
	if !strings.Contains(out, "could not be signed") {
		t.Errorf("the failure does not mention signing:\n%s", out)
	}
}

// The checksum write used to be `|| true` as well.
func TestReleaseFailsWhenTheChecksumToolFails(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_SHASUM_FAIL"] = "1"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release with no working SHA-256 tool exited 0:\n%s", out)
	}
	if !strings.Contains(out, "SHA256SUMS") {
		t.Errorf("the failure does not mention the checksum file:\n%s", out)
	}
}

// A target that "built" without leaving a file behind is the same as a missing
// target: the checksum file would cover three artifacts while claiming four.
func TestReleaseFailsWhenATargetProducesNoArtifact(t *testing.T) {
	s := newScriptDir(t)

	env := crossCompilers()
	env["STUB_SKIP_ARTIFACT"] = "1"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release with empty artifacts exited 0:\n%s", out)
	}
	if !strings.Contains(out, "empty artifact") {
		t.Errorf("the failure does not mention the empty artifact:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Errorf("an incomplete release still wrote SHA256SUMS")
	}
}

// The declared target list is the contract; a stale artifact from an earlier run
// must not be able to leak into the checksum file.
func TestChecksumsCoverOnlyThisBuild(t *testing.T) {
	s := newScriptDir(t)

	stale := filepath.Join(s.out, "dustkeeper-1.0.0-phase1-linux-arm64")
	mustWrite(t, stale, "stale binary\n", 0o755)

	if _, code := s.run(t, nil); code != 0 {
		t.Fatalf("development build failed")
	}
	if strings.Contains(s.sums(t), "linux-arm64") {
		t.Errorf("SHA256SUMS covers an artifact this run did not build:\n%s", s.sums(t))
	}
}
