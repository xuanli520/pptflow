package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const productionPackageArchiveName = "harbor-factory-harbor-flow-production.tar.gz"

func TestProductionPackageScriptBuildsThreeAttestedBundlesWithoutDiscoveryMaterial(t *testing.T) {
	requireProductionPackageCommands(t)
	fixture := newProductionPackageFixture(t)
	outputs := filepath.Join(t.TempDir(), "outputs")
	if err := os.MkdirAll(outputs, 0o755); err != nil {
		t.Fatal(err)
	}

	first := filepath.Join(outputs, "first")
	runProductionPackage(t, fixture, first, nil)
	assertUnifiedProductionPackage(t, first)
	assertUnifiedProductionBuildToolInvocation(t, fixture)

	second := filepath.Join(outputs, "second")
	runProductionPackage(t, fixture, second, nil)
	assertUnifiedProductionPackage(t, second)
	for _, name := range []string{productionPackageArchiveName, "SHA256SUMS"} {
		firstBytes, err := os.ReadFile(filepath.Join(first, name))
		if err != nil {
			t.Fatal(err)
		}
		secondBytes, err := os.ReadFile(filepath.Join(second, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstBytes, secondBytes) {
			t.Fatalf("%s is not reproducible across equivalent builds", name)
		}
	}

	for _, name := range []string{"existing-file", "existing-directory"} {
		output := filepath.Join(outputs, name)
		if name == "existing-file" {
			if err := os.WriteFile(output, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		if outputText, err := runProductionPackageResult(fixture, output, nil); err == nil {
			t.Fatalf("existing output %q was accepted", name)
		} else if !strings.Contains(outputText, "refusing to replace existing or symlink output target") {
			t.Fatalf("existing output %q failed for an unexpected reason: %s", name, outputText)
		}
	}

	symlinkOutput := filepath.Join(outputs, "existing-symlink")
	if err := os.Symlink(filepath.Join(outputs, "symlink-target"), symlinkOutput); err != nil {
		t.Fatal(err)
	}
	if outputText, err := runProductionPackageResult(fixture, symlinkOutput, nil); err == nil {
		t.Fatal("symlink output was accepted")
	} else if !strings.Contains(outputText, "refusing to replace existing or symlink output target") {
		t.Fatalf("symlink output failed for an unexpected reason: %s", outputText)
	}
	if info, err := os.Lstat(symlinkOutput); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink output was altered: info=%v err=%v", info, err)
	}

	failedOutput := filepath.Join(outputs, "failed-build")
	if _, err := runProductionPackageResult(fixture, failedOutput, map[string]string{"FAKE_GO_BUILD_FAIL": "1"}); err == nil {
		t.Fatal("failed build was accepted")
	}
	assertNoPublishedOutputOrStaging(t, outputs, failedOutput)

	dirtyDuringBuild := filepath.Join(outputs, "dirty-during-build")
	if outputText, err := runProductionPackageResult(fixture, dirtyDuringBuild, map[string]string{"FAKE_GIT_DIRTY_ON_STATUS_CALL": "2"}); err == nil {
		t.Fatal("source changed during packaging was accepted")
	} else if !strings.Contains(outputText, "a clean committed source tree is required") {
		t.Fatalf("source-change packaging failure = %q, want clean-source rejection", outputText)
	}
	assertNoPublishedOutputOrStaging(t, outputs, dirtyDuringBuild)
}

func TestProductionPackageScriptRequiresAllThreeCatalogLockPairs(t *testing.T) {
	requireProductionPackageCommands(t)
	fixture := newProductionPackageFixture(t)
	if err := os.Remove(filepath.Join(fixture.root, "deployments", "codeedge-phase1", "operation-catalog.lock.json")); err != nil {
		t.Fatal(err)
	}
	outputText, err := runProductionPackageResult(fixture, filepath.Join(t.TempDir(), "missing-parent-lock"), nil)
	if err == nil || !strings.Contains(outputText, "CodeEdge Phase-1 lock must be a regular non-symlink file") {
		t.Fatalf("missing parent lock error = %q, want explicit three-bundle lock rejection", outputText)
	}
}

type productionPackageFixture struct {
	root            string
	script          string
	fakeBin         string
	gitStatusState  string
	goInvocationLog string
}

func newProductionPackageFixture(t *testing.T) productionPackageFixture {
	t.Helper()
	root := t.TempDir()
	script := filepath.Join(root, "scripts", "build-codeedge-production.sh")
	copyPackagingFixtureFile(t, filepath.Join("..", "..", "scripts", "build-codeedge-production.sh"), script, 0o755)

	writeProductionBundleFixture(t, root, "standard-authoring", []string{
		"README.md", "operation-catalog.v1.json", "operation-catalog.lock.json", "contract-assets.v1.json", "execution-profile.v1.json",
		"prompts/task-design.json", "schemas/codex-turn-output.json",
	})
	writeProductionBundleFixture(t, root, "codeedge-phase1", []string{
		"README.md", "operation-catalog.v1.json", "operation-catalog.lock.json",
	})
	writeProductionBundleFixture(t, root, "codeedge-evaluator-child", []string{
		"README.md", "operation-catalog.v1.json", "operation-catalog.lock.json",
		"candidates/README.md", "candidates/operation-inventory.discovery.v1.json",
	})

	fakeBin := filepath.Join(root, "fake-bin")
	writePackagingFixtureFile(t, filepath.Join(fakeBin, "git"), []byte(`#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "-C" ]]; then
  shift 2
fi
case "${1:-}" in
  status)
    state="${FAKE_GIT_STATUS_STATE:?}"
    count=0
    if [[ -f "$state" ]]; then
      count="$(<"$state")"
    fi
    count=$((count + 1))
    printf '%s\n' "$count" > "$state"
    if [[ -n "${FAKE_GIT_DIRTY_ON_STATUS_CALL:-}" && "$count" -ge "$FAKE_GIT_DIRTY_ON_STATUS_CALL" ]]; then
      printf ' M changed-during-build\n'
    fi
    ;;
  rev-parse)
    printf '%s\n' '0123456789012345678901234567890123456789'
    ;;
  show)
    printf '%s\n' '1700000000'
    ;;
  ls-tree)
    printf '100644 blob 0123456789012345678901234567890123456789\tgo.mod\n'
    ;;
  check-ignore)
    exit 1
    ;;
  *)
    printf 'unexpected fake git command: %s\n' "${1:-}" >&2
    exit 64
    ;;
esac
`), 0o755)
	writePackagingFixtureFile(t, filepath.Join(fakeBin, "go"), []byte(`#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  run)
    printf '%s\n' "$*" >> "${FAKE_GO_INVOCATION_LOG:?}"
    printf '%s\n' '-X fixture.build=value'
    ;;
  build)
    if [[ "${FAKE_GO_BUILD_FAIL:-}" == "1" ]]; then
      exit 42
    fi
    shift
    output=""
    while (($#)); do
      if [[ "$1" == "-o" ]]; then
        output="${2:-}"
        shift 2
        continue
      fi
      shift
    done
    [[ -n "$output" ]]
    printf '%s\n' '#!/usr/bin/env sh' 'printf fixture' > "$output"
    chmod 0755 "$output"
    ;;
  *)
    printf 'unexpected fake go command: %s\n' "${1:-}" >&2
    exit 64
    ;;
esac
`), 0o755)

	return productionPackageFixture{
		root: root, script: script, fakeBin: fakeBin,
		gitStatusState: filepath.Join(root, "fake-git-status-state"), goInvocationLog: filepath.Join(root, "fake-go-invocations"),
	}
}

func writeProductionBundleFixture(t *testing.T, root, bundleName string, files []string) {
	t.Helper()
	for _, relativePath := range files {
		writePackagingFixtureFile(t, filepath.Join(root, "deployments", bundleName, relativePath), []byte("fixture "+relativePath+"\n"), 0o644)
	}
}

func requireProductionPackageCommands(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the production package is a POSIX/GNU-tar artifact")
	}
	for _, command := range []string{"bash", "find", "tar", "gzip", "sha256sum", "mv", "install"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Skipf("%s is required for the production package test: %v", command, err)
		}
	}
}

func copyPackagingFixtureFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	writePackagingFixtureFile(t, destination, contents, mode)
}

func writePackagingFixtureFile(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func runProductionPackage(t *testing.T, fixture productionPackageFixture, output string, extraEnvironment map[string]string) {
	t.Helper()
	outputText, err := runProductionPackageResult(fixture, output, extraEnvironment)
	if err != nil {
		t.Fatalf("production package script failed: %v\n%s", err, outputText)
	}
}

func runProductionPackageResult(fixture productionPackageFixture, output string, extraEnvironment map[string]string) (string, error) {
	_ = os.Remove(fixture.gitStatusState)
	_ = os.Remove(fixture.goInvocationLog)
	command := exec.Command("bash", fixture.script, output)
	command.Dir = fixture.root
	command.Env = append(os.Environ(),
		"PATH="+fixture.fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GIT_STATUS_STATE="+fixture.gitStatusState,
		"FAKE_GO_INVOCATION_LOG="+fixture.goInvocationLog,
	)
	for key, value := range extraEnvironment {
		command.Env = append(command.Env, key+"="+value)
	}
	outputText, err := command.CombinedOutput()
	return string(outputText), err
}

func assertUnifiedProductionBuildToolInvocation(t *testing.T, fixture productionPackageFixture) {
	t.Helper()
	invocations, err := os.ReadFile(fixture.goInvocationLog)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(invocations))
	for _, want := range []string{
		"run -mod=readonly ./tools/harbor-flow-production-build",
		"--standard-authoring-catalog ", "--standard-authoring-lock ",
		"--codeedge-phase1-catalog ", "--codeedge-phase1-lock ",
		"--codeedge-evaluator-catalog ", "--codeedge-evaluator-lock ",
		"--source-manifest sha256:",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("unified production build tool invocation %q omits %q", line, want)
		}
	}
	if strings.Contains(line, "codeedge-production-build") {
		t.Fatalf("package script invoked obsolete evaluator-only build tool: %q", line)
	}
}

func assertUnifiedProductionPackage(t *testing.T, output string) {
	t.Helper()
	for path, mode := range map[string]os.FileMode{
		"harbor-factory": 0o755,
		"deployments":    0o755,
		"SHA256SUMS":     0o644,
	} {
		info, err := os.Stat(filepath.Join(output, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}

	checksumCommand := exec.Command("sha256sum", "-c", "SHA256SUMS")
	checksumCommand.Dir = output
	if checksumOutput, err := checksumCommand.CombinedOutput(); err != nil {
		t.Fatalf("SHA256SUMS does not verify: %v\n%s", err, checksumOutput)
	}
	checksums, err := os.ReadFile(filepath.Join(output, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range productionPackagePayloads() {
		if !strings.Contains(string(checksums), "  "+name+"\n") {
			t.Fatalf("SHA256SUMS omits %s:\n%s", name, checksums)
		}
	}
	if strings.Contains(string(checksums), "  SHA256SUMS\n") {
		t.Fatalf("SHA256SUMS must not contain a self-referential checksum:\n%s", checksums)
	}

	archiveCommand := exec.Command("tar", "-tzf", filepath.Join(output, productionPackageArchiveName))
	archiveOutput, err := archiveCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("read deterministic archive: %v\n%s", err, archiveOutput)
	}
	archiveContents := string(archiveOutput)
	for _, name := range append([]string{"harbor-factory"}, productionPackagePayloads()[:len(productionPackagePayloads())-1]...) {
		if !strings.Contains(archiveContents, name+"\n") {
			t.Fatalf("archive omits %s:\n%s", name, archiveContents)
		}
	}
	for _, forbidden := range []string{"candidates/", "discovery", "SHA256SUMS"} {
		if strings.Contains(archiveContents, forbidden) {
			t.Fatalf("archive contains forbidden non-production material %q:\n%s", forbidden, archiveContents)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "deployments", "codeedge-evaluator-child", "candidates")); !os.IsNotExist(err) {
		t.Fatalf("candidate discovery directory was packaged: %v", err)
	}
}

func productionPackagePayloads() []string {
	return []string{
		"deployments/codeedge-evaluator-child/README.md",
		"deployments/codeedge-evaluator-child/operation-catalog.lock.json",
		"deployments/codeedge-evaluator-child/operation-catalog.v1.json",
		"deployments/codeedge-phase1/README.md",
		"deployments/codeedge-phase1/operation-catalog.lock.json",
		"deployments/codeedge-phase1/operation-catalog.v1.json",
		"deployments/standard-authoring/README.md",
		"deployments/standard-authoring/contract-assets.v1.json",
		"deployments/standard-authoring/execution-profile.v1.json",
		"deployments/standard-authoring/operation-catalog.lock.json",
		"deployments/standard-authoring/operation-catalog.v1.json",
		"deployments/standard-authoring/prompts/task-design.json",
		"deployments/standard-authoring/schemas/codex-turn-output.json",
		"harbor-factory",
		productionPackageArchiveName,
	}
}

func assertNoPublishedOutputOrStaging(t *testing.T, outputs, output string) {
	t.Helper()
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatalf("failed build published an output target: %v", err)
	}
	leftovers, err := filepath.Glob(filepath.Join(outputs, ".harbor-flow-production.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("failed build left staging directories: %v", leftovers)
	}
}
