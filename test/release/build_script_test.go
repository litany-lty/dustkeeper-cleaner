// Package release exercises build/build.sh as a reviewer would: by running it.
//
// The script is the only thing standing between the source tree and a published
// binary, so "it exits 0" has to mean "the artifact for this host exists, is
// signed where the platform needs it, runs the two actions the product is for, and
// is covered by a checksum that verifies". These tests drive the script with a stub
// toolchain, so the gate can be tested without compiling anything.
//
// The model under test is one target per run: the host's. A host that is not one of
// the two release targets cannot produce a release, and that refusal is what the
// first two cases pin.
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

// stubGo answers "go env", "go version" and "go build". The artifact it writes is a
// shell stand-in that speaks just enough of the CLI for the run smoke: without it
// the smoke step could only be tested by not testing it.
const stubGo = `#!/bin/sh
if [ "${1:-}" = "env" ]; then
	case "${2:-}" in
	GOOS) echo "${STUB_GOOS:-$STUB_HOST_OS}" ;;
	GOARCH) echo "${STUB_GOARCH:-$STUB_HOST_ARCH}" ;;
	GOHOSTOS) echo "$STUB_HOST_OS" ;;
	GOHOSTARCH) echo "$STUB_HOST_ARCH" ;;
	*) echo "" ;;
	esac
	exit 0
fi
if [ "${1:-}" = "version" ]; then
	echo "go version go1.99 stub/os"
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
	cat >"$out" <<'ARTIFACT'
#!/bin/sh
# A stand-in for dustkeeper: version plus the quarantine/restore flow.
#
# The quarantine is modelled as the product behaves in the two places the smoke
# now checks independently: "plan apply" takes the source out of the tree, and
# "quarantine restore" puts those bytes back. STUB_QR_NOOP and STUB_QR_BAD_BYTES
# break exactly one of those while every command still exits 0 -- the two shapes
# of failure a smoke that only reads JSON would call a success.
vault="${DUSTKEEPER_STATE_DIR%/}/stub-vault"
tree=$(cat "${DUSTKEEPER_STATE_DIR%/}/stub-tree" 2>/dev/null)
case "${1:-}" in
version) echo "dustkeeper stub (stub/os)" ;;
init) echo '{"status":"ok","data":{}}' ;;
scope)
	case "${2:-}" in
	add)
		shift 2
		while [ $# -gt 0 ]; do
			case "$1" in
			--path) printf '%s' "$2" >"${DUSTKEEPER_STATE_DIR%/}/stub-tree"; tree=$2; shift 2 ;;
			*) shift ;;
			esac
		done
		echo '{"status":"ok","data":{"scope_id":"scp_stub"}}' ;;
	*) exit 2 ;;
	esac
	;;
scan) echo '{"status":"ok","data":{"scan":{},"scan_state":{}}}' ;;
rule)
	case "${2:-}" in
	import|activate) echo '{"status":"ok","data":{}}' ;;
	preview) echo '{"status":"ok","data":{"review_digest":"sha256:stub"}}' ;;
	*) exit 2 ;;
	esac
	;;
plan)
	case "${2:-}" in
	create) echo '{"status":"ok","data":{"plan_id":"pln_stub","digest":"sha256:stub"}}' ;;
	apply)
		mkdir -p "$vault"
		: >"$vault/obj_stub"
		if [ "${STUB_QR_NOOP:-0}" != "1" ] && [ -n "$tree" ] && [ -f "$tree/old.zip" ]; then
			mv "$tree/old.zip" "$vault/old.zip"
		fi
		echo '{"status":"ok","data":{}}'
		;;
	*) exit 2 ;;
	esac
	;;
quarantine)
	case "${2:-}" in
	list)
		if [ "${STUB_BROKEN_QR:-0}" = "1" ] || [ ! -f "$vault/obj_stub" ]; then
			echo '{"status":"ok","data":{"objects":[]}}'
		else
			echo '{"status":"ok","data":{"objects":[{"item_id":"obj_stub","state":"quarantined","restorable":true}]}}'
		fi
		;;
	restore)
		if [ -n "$tree" ] && [ -f "$vault/old.zip" ]; then
			if [ "${STUB_QR_BAD_BYTES:-0}" = "1" ]; then
				printf 'something else entirely\n' >"$tree/old.zip"
				rm -f "$vault/old.zip"
			else
				mv "$vault/old.zip" "$tree/old.zip"
			fi
		fi
		rm -f "$vault/obj_stub"
		echo '{"status":"ok","data":{}}'
		;;
	*) exit 2 ;;
	esac
	;;
*) exit 2 ;;
esac
ARTIFACT
	chmod 755 "$out"
	exit 0
fi
echo "stub go: unsupported invocation: $*" >&2
exit 2
`

// stubCC answers the questions the libc policy asks a compiler: which triple it
// targets, which version it is, and -- the part the policy actually reads -- what
// the link would be made of. The policy reads the startfile and the -L that the
// driver passes to collect2 during a static link, because that is the only answer
// the compiler cannot be talked into by CPATH, a banner or -print-file-name. All of
// it is derived from the triple, so a stub that claims a glibc triple really answers
// like glibc and one that claims musl answers like musl; a test that wants to see
// how the policy treats a noisy transcript can override the banner, the sysroot and
// the include list without changing the link.
const stubCC = `#!/bin/sh
t=${STUB_CC_TRIPLE:-x86_64-linux-musl}
case "${1:-}" in
-dumpmachine) echo "$t" ;;
--version) echo "${STUB_CC_VERSION:-stub cc 1.0 for tests}" ;;
-print-sysroot) echo "${STUB_CC_SYSROOT:-/}" ;;
-print-file-name)
	case "${2:-}" in
	libc.a) echo "${STUB_CC_LIBC_A:-/usr/lib/$t/libc.a}" ;;
	*) echo "${2:-}" ;;
	esac ;;
-v)
	{
		echo "${STUB_CC_VERSION:-stub cc 1.0 for tests}"
		echo "${STUB_CC_BANNER:-}"
		echo "#include <...> search starts here:"
		for d in ${STUB_CC_INCLUDE:-/usr/include/$t}; do echo " $d"; done
		echo "End of search list."
	} >&2
	exit 0 ;;
-static)
	if [ "${STUB_CC_LINK_FAIL:-0}" = "1" ]; then
		echo "stub cc: collect2: cannot find -lc" >&2
		exit 1
	fi
	out=""
	prev=""
	for a in "$@"; do
		if [ "$prev" = "-o" ]; then out="$a"; fi
		prev="$a"
	done
	if [ -n "$out" ]; then : >"$out"; fi
	{
		echo "${STUB_CC_VERSION:-stub cc 1.0 for tests}"
		echo "#include <...> search starts here:"
		for d in ${STUB_CC_INCLUDE:-/usr/include/$t}; do echo " $d"; done
		echo "End of search list."
		echo " /usr/lib/gcc/$t/collect2 -static -o ${out:-a.out} ${STUB_CC_STARTFILE:-/usr/lib/$t/Scrt1.o} ${STUB_CC_LIBDIR_ARG:--L/usr/lib/$t} -lc"
	} >&2
	exit 0 ;;
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
for a in "$@"; do
	if [ "$a" = "-c" ] && [ "${STUB_SHASUM_FAILCHECK:-0}" = "1" ]; then
		echo "stub shasum: WARNING: 1 computed checksum did NOT match" >&2
		exit 1
	fi
done
for c in /usr/bin/shasum /usr/bin/sha256sum /bin/sha256sum; do
	if [ -x "$c" ]; then exec "$c" "$@"; fi
done
echo "stub shasum: no real implementation available" >&2
exit 127
`

// newScriptDir copies the real build script into a throwaway tree and installs stub
// tools ahead of the real ones.
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
// PATH, no inherited compiler variables, and a stubbed host platform.
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

// linuxHost is a machine whose release target is linux-amd64, with a musl compiler.
func linuxHost() map[string]string {
	return map[string]string{
		"STUB_HOST_OS":   "linux",
		"STUB_HOST_ARCH": "amd64",
		"CC":             "stub-cc",
	}
}

func (s *scriptDir) sums(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.out, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read SHA256SUMS: %v", err)
	}
	return string(body)
}

func (s *scriptDir) manifest(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.out, "release-manifest.txt"))
	if err != nil {
		t.Fatalf("read release manifest: %v", err)
	}
	return string(body)
}

func (s *scriptDir) facts(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(s.out, "build-facts.txt"))
	if err != nil {
		t.Fatalf("read build facts: %v", err)
	}
	return string(body)
}

// A host that is not a release target must not produce a release. The old gate
// failed because three of four targets could not be cross-compiled; the failure
// this one exists for is a machine publishing something it never proved.
func TestReleaseFailsOnAHostThatIsNotAReleaseTarget(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_HOST_OS": "linux", "STUB_HOST_ARCH": "arm64"}, "--release")
	if code == 0 {
		t.Fatalf("a release on linux/arm64 exited 0:\n%s", out)
	}
	for _, want := range []string{"linux-arm64 is not a release target", "darwin-arm64", "linux-amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("a refused release still wrote SHA256SUMS")
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a refused release still wrote a manifest")
	}
}

// The same host may still build for itself in development. It just does not get
// the label, and there is no manifest to cite later. The compiler is a musl one so
// that this is about the target label, not about the libc policy.
func TestDevelopmentBuildOnAnUnsupportedHostIsNotARelease(t *testing.T) {
	s := newScriptDir(t)

	env := map[string]string{
		"STUB_HOST_OS":   "linux",
		"STUB_HOST_ARCH": "arm64",
		"CC":             "stub-cc",
		"STUB_CC_TRIPLE": "aarch64-linux-musl",
	}
	out, code := s.run(t, env)
	if code != 0 {
		t.Fatalf("a development build on linux/arm64 failed:\n%s", out)
	}
	if !strings.Contains(out, "not a release target") || !strings.Contains(out, "not labelled supported") {
		t.Errorf("the development build did not say what it is:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "dustkeeper-1.0.0-phase1-linux-arm64")); err != nil {
		t.Errorf("the host target was not built: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a development build wrote a release manifest")
	}
	if !strings.Contains(s.sums(t), "dustkeeper-1.0.0-phase1-linux-arm64") {
		t.Errorf("SHA256SUMS omits the artifact this run built:\n%s", s.sums(t))
	}
}

// The happy path has to hold, otherwise the gate is just "always fail".
func TestReleaseSucceedsOnASupportedHost(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, nil, "--release")
	if code != 0 {
		t.Fatalf("a release on darwin/arm64 exited %d:\n%s", code, out)
	}
	artifact := filepath.Join(s.out, "dustkeeper-1.0.0-phase1-darwin-arm64")
	info, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("the artifact is missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the artifact is empty")
	}
	if lines := strings.Count(strings.TrimSpace(s.sums(t)), "\n") + 1; lines != 1 {
		t.Errorf("SHA256SUMS covers %d artifacts, want 1:\n%s", lines, s.sums(t))
	}

	m := s.manifest(t)
	for _, want := range []string{
		"target=darwin-arm64",
		"host=darwin-arm64",
		"supported_targets=",
		"artifact=dustkeeper-1.0.0-phase1-darwin-arm64",
		"sha256=",
		"go=",
		"build_env=",
		"smoke=ok",
		"smoke_qr=ok",
		"linux_libc=not-applicable",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the release manifest has no %q:\n%s", want, m)
		}
	}
	// The manifest's checksum is the artifact's, not a claim about it.
	sum := strings.Fields(s.sums(t))[0]
	if !strings.Contains(m, "sha256="+sum) {
		t.Errorf("the manifest does not carry the checksum that SHA256SUMS lists (%s):\n%s", sum, m)
	}
}

// A Linux release without a musl proof is refused: "-static" says nothing about
// which libc was linked, and a glibc static binary is not this policy's artifact.
func TestLinuxReleaseRequiresAMuslProof(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a release built with a glibc compiler exited 0:\n%s", out)
	}
	if !strings.Contains(out, "not musl") || !strings.Contains(out, "linux-amd64") {
		t.Errorf("the failure does not name the target and the reason:\n%s", out)
	}
	if !strings.Contains(out, "x86_64-linux-gnu") {
		t.Errorf("the failure does not quote the compiler's own triple:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a release without a libc proof still wrote a manifest")
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("a release without a libc proof still wrote SHA256SUMS")
	}
}

// A transcript that merely mentions musl is not a proof. A banner line or a
// -print-sysroot answer can say anything while the compiler still searches glibc's
// headers and links glibc's libc.a; the policy reads the include search list and
// the libc.a, so this compiler stays unproved.
func TestAMuslMentionIsNotAMuslProof(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["STUB_CC_VERSION"] = "stub cc 1.0, musl-flavoured for tests"
	env["STUB_CC_SYSROOT"] = "/usr/musl"
	env["STUB_CC_BANNER"] = "configured with --with-libc=/usr/musl/lib/libc.a"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a glibc compiler whose transcript mentions musl produced a release:\n%s", out)
	}
	if !strings.Contains(out, "not musl") {
		t.Errorf("the failure does not state the libc policy:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a release without a libc proof still wrote a manifest")
	}
}

// The other direction: Debian's musl-gcc reports a gnu triple and is still musl.
// The values here are the ones that compiler really gives (measured in the pinned
// builder image): a gnu triple, musl's include directory, and -- the part that
// makes libc.a useless as evidence -- glibc's archive path from
// -print-file-name=libc.a, because musl's specs move the real -lc elsewhere. What
// does name musl is the link: Scrt1.o from musl's directory and the -L that goes
// with it. That compiler must be provable, or the fixed builder image could never
// pass its own gate.
func TestDebianMuslGccIsProvedByItsLinkInputs(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["STUB_CC_INCLUDE"] = "/usr/include/x86_64-linux-musl /usr/lib/gcc/x86_64-linux-gnu/12/include"
	env["STUB_CC_LIBC_A"] = "/usr/lib/gcc/x86_64-linux-gnu/12/../../../x86_64-linux-gnu/libc.a"
	env["STUB_CC_STARTFILE"] = "/usr/lib/x86_64-linux-musl/Scrt1.o"
	env["STUB_CC_LIBDIR_ARG"] = "-L/usr/lib/x86_64-linux-musl"
	out, code := s.run(t, env, "--release")
	if code != 0 {
		t.Fatalf("a musl-gcc-style compiler was refused a release:\n%s", out)
	}
	m := s.manifest(t)
	if !strings.Contains(m, "linkage=static") {
		t.Errorf("the manifest does not record a static artifact:\n%s", m)
	}
	if !strings.Contains(m, "musl_evidence=/usr/lib/x86_64-linux-musl/Scrt1.o") {
		t.Errorf("the manifest does not record the evidence it accepted:\n%s", m)
	}
	if !strings.Contains(m, "musl_libdir=-L/usr/lib/x86_64-linux-musl") {
		t.Errorf("the manifest does not record the library search path:\n%s", m)
	}
}

// A startfile alone is not the proof either: the -lc that goes with it has to come
// from musl too, or the binary is glibc's with a borrowed crt1.o.
func TestAMuslStartfileWithoutAMuslLibraryPathIsRefused(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["STUB_CC_STARTFILE"] = "/usr/lib/x86_64-linux-musl/Scrt1.o"
	env["STUB_CC_LIBDIR_ARG"] = "-L/usr/lib/x86_64-linux-gnu"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("a compiler with a musl startfile and glibc's -L produced a release:\n%s", out)
	}
	if !strings.Contains(out, "not musl") {
		t.Errorf("the failure does not state the libc policy:\n%s", out)
	}
}

// D14.1: the counterexample the review used. A real glibc compiler, plus one empty
// directory named after musl, added to the include search list (CPATH is how it gets
// there). features.h is glibc's and libc.a is glibc's; only the link says glibc, and
// that is what the policy reads.
func TestAnEmptyMuslNamedIncludeDirectoryIsNotAProof(t *testing.T) {
	s := newScriptDir(t)

	placeholder := filepath.Join(s.root, "musl-headers-placeholder")
	if err := os.MkdirAll(placeholder, 0o755); err != nil {
		t.Fatalf("mkdir placeholder: %v", err)
	}
	if entries, err := os.ReadDir(placeholder); err != nil || len(entries) != 0 {
		t.Fatalf("the placeholder is not empty: %v %v", entries, err)
	}

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["STUB_CC_INCLUDE"] = placeholder + " /usr/include/x86_64-linux-gnu /usr/include"
	out, code := s.run(t, env, "--release")
	if code == 0 {
		t.Fatalf("an empty musl-named include directory proved a glibc compiler:\n%s", out)
	}
	for _, want := range []string{"not musl", "linux-amd64"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not name %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a release without a libc proof still wrote a manifest")
	}
}

// A compiler that cannot link the probe cannot produce the artifact, and that is a
// failure with its own message -- not a missing proof that a later step discovers.
func TestAFailedProbeLinkIsReportedAsALinkFailure(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_LINK_FAIL"] = "1"
	out, code := s.run(t, env)
	if code == 0 {
		t.Fatalf("a compiler that cannot link exited 0:\n%s", out)
	}
	if !strings.Contains(out, "static libc probe failed") {
		t.Errorf("the failure does not say the probe link failed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("an unlinkable toolchain still produced a checksum")
	}
}

// The musl path records the proof: the triple, the compiler version, and the
// artifact facts that were read from the file.
func TestLinuxReleaseRecordsTheToolchainItUsed(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, linuxHost(), "--release")
	if code != 0 {
		t.Fatalf("a musl release exited %d:\n%s", code, out)
	}
	m := s.manifest(t)
	for _, want := range []string{
		"target=linux-amd64",
		"linux_libc=musl-static",
		"linux_libc_proof=",
		"linkage=static",
		"toolchain=x86_64-linux-musl",
		"compiler_version=",
		"smoke_qr=ok",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("the manifest has no %q:\n%s", want, m)
		}
	}
}

// A development build may not silently checksum an artifact whose policy could not
// be proved: that is how an unlabelled glibc binary reaches a user. With no other
// target to build, refusing means the run produces nothing.
func TestADevelopmentBuildRefusesAnUnprovedArtifact(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	out, code := s.run(t, env)
	if code == 0 {
		t.Fatalf("a development build with a glibc compiler exited 0:\n%s", out)
	}
	if !strings.Contains(out, "not musl") {
		t.Errorf("the development build said nothing about the libc policy:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Errorf("an artifact built against glibc was checksummed:\n%s", s.sums(t))
	}
}

// A developer on a glibc machine still needs a binary. The escape hatch exists so
// that the alternative is not "edit the script": the artifact is built, labelled
// unproved, and a release refuses the same environment.
func TestADeveloperCanBuildAnUnprovedLinuxArtifactExplicitly(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
	env["STUB_CC_TRIPLE"] = "x86_64-linux-gnu"
	env["ALLOW_UNPROVED_LIBC"] = "1"

	out, code := s.run(t, env)
	if code != 0 {
		t.Fatalf("an explicit unproved development build failed:\n%s", out)
	}
	if !strings.Contains(out, "linkage is unproved") {
		t.Errorf("the build did not say the linkage is unproved:\n%s", out)
	}
	facts := s.facts(t)
	if !strings.Contains(facts, "target=linux-amd64 format=elf arch=amd64 linkage=unproved") {
		t.Errorf("the facts do not record the unproved linkage:\n%s", facts)
	}
	if strings.Contains(facts, "linkage=static toolchain=x86_64-linux-gnu") {
		t.Errorf("a glibc build was recorded as a static musl artifact:\n%s", facts)
	}
	if !strings.Contains(s.sums(t), "dustkeeper-1.0.0-phase1-linux-amd64") {
		t.Errorf("the development artifact was not checksummed:\n%s", s.sums(t))
	}

	// The same environment cannot produce a release, whatever the variable says.
	if out, code := s.run(t, env, "--release"); code == 0 {
		t.Fatalf("ALLOW_UNPROVED_LIBC produced a release:\n%s", out)
	}
}

// The artifact itself has to be read, not inferred from the compiler.
func TestReleaseFailsWhenTheArtifactIsNotStatic(t *testing.T) {
	s := newScriptDir(t)

	env := linuxHost()
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

	out, code := s.run(t, map[string]string{"STUB_CODESIGN_FAIL": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release with an unsigned macOS binary exited 0:\n%s", out)
	}
	if !strings.Contains(out, "could not be signed") {
		t.Errorf("the failure does not mention signing:\n%s", out)
	}
}

// The checksum write used to be `|| true` as well.
func TestReleaseFailsWhenTheChecksumToolFails(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_SHASUM_FAIL": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release with no working SHA-256 tool exited 0:\n%s", out)
	}
	if !strings.Contains(out, "SHA256SUMS") {
		t.Errorf("the failure does not mention the checksum file:\n%s", out)
	}
}

// A build step that returns 0 without leaving a file behind is a missing artifact:
// the checksum file would be empty while the build reported success.
func TestReleaseFailsWhenTheTargetProducesNoArtifact(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_SKIP_ARTIFACT": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release with an empty artifact exited 0:\n%s", out)
	}
	if !strings.Contains(out, "empty artifact") {
		t.Errorf("the failure does not mention the empty artifact:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("an incomplete release still wrote SHA256SUMS")
	}
}

// A stale artifact from an earlier run must not leak into the checksum file: the
// checksum file describes this build.
func TestChecksumsCoverOnlyThisBuild(t *testing.T) {
	s := newScriptDir(t)

	stale := filepath.Join(s.out, "dustkeeper-1.0.0-phase1-linux-amd64")
	mustWrite(t, stale, "stale binary\n", 0o755)

	if _, code := s.run(t, nil, "--release"); code != 0 {
		t.Fatalf("the release failed")
	}
	if strings.Contains(s.sums(t), "linux-amd64") {
		t.Errorf("SHA256SUMS covers an artifact this run did not build:\n%s", s.sums(t))
	}
}

// The run smoke is what says the artifact performs the two actions the product is
// for, so a release must not be produced when it fails.
func TestReleaseFailsWhenTheRunSmokeFails(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_BROKEN_QR": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release whose artifact could not carry out a quarantine exited 0:\n%s", out)
	}
	if !strings.Contains(out, "quarantine/restore smoke") {
		t.Errorf("the failure does not mention the smoke:\n%s", out)
	}
	if !strings.Contains(out, "nothing was listed") {
		t.Errorf("the smoke log does not say what went wrong:\n%s", out)
	}
}

// SKIP_SMOKE is a development convenience. A release ignores it: an artifact that
// was never run is not a released artifact.
func TestAReleaseIgnoresSkipSmoke(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"SKIP_SMOKE": "1"}, "--release")
	if code != 0 {
		t.Fatalf("a release with SKIP_SMOKE=1 exited %d:\n%s", code, out)
	}
	m := s.manifest(t)
	if strings.Contains(m, "smoke=skipped") {
		t.Fatalf("a release recorded a skipped smoke:\n%s", m)
	}
	if !strings.Contains(m, "smoke_qr=ok") {
		t.Fatalf("a release did not record the run smoke:\n%s", m)
	}
}

// build-linux-release.sh hands the container one single-quoted shell word. An
// apostrophe inside that body ends the word early: the rest of the script then runs
// on the host, which is how "the container's /tmp" once turned into a mkdir of
// /smoke on macOS. The body is therefore read back and checked for exactly that.
func TestTheContainerBodyIsOneShellWord(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "build", "build-linux-release.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read build-linux-release.sh: %v", err)
	}
	script := string(body)

	open := strings.Index(script, "sh -c '")
	if open < 0 {
		t.Fatal("build-linux-release.sh has no container body")
	}
	rest := script[open+len("sh -c '"):]
	end := strings.Index(rest, "\n'\n")
	if end < 0 {
		t.Fatal("the container body is not terminated by a line holding a single quote")
	}
	container := rest[:end]

	if strings.Contains(container, "'") {
		t.Errorf("the container body contains an apostrophe, which ends the shell word early:\n%s",
			container)
	}
	if !strings.Contains(container, "./build/build.sh --release") {
		t.Errorf("the container body does not run the release build:\n%s", container)
	}
	if !strings.Contains(container, "TMPDIR=") {
		t.Errorf("the container body does not point TMPDIR at the smoke volume:\n%s", container)
	}
}

// D14.2: a smoke that only reads the tool's own JSON accepts a quarantine that
// moved nothing. The step that reported success is the step being checked, so the
// tree itself has to be looked at.
func TestTheSmokeFailsWhenNothingIsMoved(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_QR_NOOP": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a quarantine that left the source in place produced a release:\n%s", out)
	}
	if !strings.Contains(out, "still at its original path") {
		t.Errorf("the failure does not say the source was never moved:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a failed smoke still wrote a release manifest")
	}
}

// And the other half: a restore that writes some other file's bytes satisfies "the
// path exists again". The bytes that come back have to be the bytes that went in.
func TestTheSmokeFailsWhenTheRestoreWritesOtherBytes(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_QR_BAD_BYTES": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a restore that wrote the wrong content produced a release:\n%s", out)
	}
	if !strings.Contains(out, "differs from the file that was quarantined") {
		t.Errorf("the failure does not say the content differs:\n%s", out)
	}
}

// A restore also has to retire the object's record: an item that is back on disk
// but still listed as restorable is a state nobody can act on.
func TestTheSmokeChecksTheObjectRecordAfterARestore(t *testing.T) {
	s := newScriptDir(t)

	if out, code := s.run(t, nil, "--release"); code != 0 {
		t.Fatalf("the release failed:\n%s", out)
	}
	facts := s.facts(t)
	for _, want := range []string{
		"source_gone_after_apply=yes",
		"restored_bytes_match=yes",
		"quarantine restore,quarantine list",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("the facts do not record %q:\n%s", want, facts)
		}
	}
}

// D14.3: a run that could not write its checksum file used to leave its binary in
// dist, and the previous run's manifest next to it. What is left is a directory that
// looks like a release, so the failure has to take its own outputs with it.
func TestAFailedChecksumLeavesNoArtifactAndNoManifest(t *testing.T) {
	s := newScriptDir(t)

	// The previous run's claim, and its binary, are in place before this one starts.
	oldManifest := filepath.Join(s.out, "release-manifest.txt")
	oldArtifact := filepath.Join(s.out, "dustkeeper-0.9.0-darwin-arm64")
	mustWrite(t, oldManifest, "version=0.9.0\nartifact=dustkeeper-0.9.0-darwin-arm64\n", 0o644)
	mustWrite(t, oldArtifact, "an earlier release\n", 0o755)

	out, code := s.run(t, map[string]string{"STUB_SHASUM_FAIL": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release whose checksum tool failed exited 0:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "dustkeeper-1.0.0-phase1-darwin-arm64")); err == nil {
		t.Error("this run's binary survived a failure that verified nothing")
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("a checksum file this run could not fill was left in dist")
	}
	if _, err := os.Stat(oldManifest); err == nil {
		t.Error("an earlier run's manifest was left describing a release this run did not produce")
	}
	// The other version's binary is not this run's output and is not its to delete.
	if _, err := os.Stat(oldArtifact); err != nil {
		t.Errorf("an unrelated earlier artifact was deleted: %v", err)
	}
}

// A checksum file that exists but does not verify is worse than none: it is an
// artifact plus a claim that the artifact is good.
func TestAChecksumFileThatDoesNotVerifyIsRemoved(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_SHASUM_FAILCHECK": "1"}, "--release")
	if code == 0 {
		t.Fatalf("a release whose checksum file did not verify exited 0:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(s.out, "SHA256SUMS")); err == nil {
		t.Error("an unverified checksum file was left in dist")
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a manifest was written from an unverified checksum")
	}
}

// D14.5: GOOS/GOARCH are target settings. A build that targets another system must
// not be named after the machine it happens to run on.
func TestACrossCompiledSettingIsRefused(t *testing.T) {
	s := newScriptDir(t)

	out, code := s.run(t, map[string]string{"STUB_GOOS": "linux", "STUB_GOARCH": "amd64"}, "--release")
	if code == 0 {
		t.Fatalf("a toolchain targeting linux-amd64 on a darwin-arm64 host exited 0:\n%s", out)
	}
	for _, want := range []string{"targets linux-amd64", "runs on darwin-arm64", "unset GOOS/GOARCH"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(s.out, "release-manifest.txt")); err == nil {
		t.Error("a cross-compiled setting produced a manifest")
	}
}
