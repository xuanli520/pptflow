package authoringharness

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	DockerfileRelativePath  = "environment/Dockerfile"
	SolveScriptRelativePath = "solution/solve.sh"
	TestScriptRelativePath  = "tests/test.sh"
)

// Candidate is one safely read, immutable-in-memory view of the fixed files
// used by a harness mode. The same reader is used by validation and final
// artifact submission so digest semantics cannot drift between them.
type Candidate struct {
	Mode              Mode
	Dockerfile        []byte
	SolveScript       []byte
	TestScript        []byte
	CandidateDigest   workflowkit.Fingerprint
	EnvironmentDigest workflowkit.Fingerprint
}

// RelativeFiles returns the closed file set consumed by a mode.
func RelativeFiles(mode Mode) ([]string, error) {
	if err := mode.Validate(); err != nil {
		return nil, err
	}
	if mode == ModeDockerfileBuild {
		return []string{DockerfileRelativePath}, nil
	}
	return []string{DockerfileRelativePath, SolveScriptRelativePath, TestScriptRelativePath}, nil
}

// ReadCandidate reads only the mode's fixed relative files from taskRoot. It
// rejects symlinks, hardlinks, special files, oversized content and path
// replacement while a file is open.
func ReadCandidate(taskRoot string, mode Mode) (Candidate, error) {
	if err := mode.Validate(); err != nil {
		return Candidate{}, err
	}
	absolute, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absolute) != taskRoot || absolute == string(os.PathSeparator) {
		return Candidate{}, errors.New("Standard authoring harness task root is invalid")
	}
	if err := inspectCandidateDirectory(taskRoot); err != nil {
		return Candidate{}, fmt.Errorf("inspect Standard authoring harness task root: %w", err)
	}

	dockerfile, err := readCandidateFile(taskRoot, DockerfileRelativePath)
	if err != nil {
		return Candidate{}, err
	}
	var solveScript, testScript []byte
	if mode == ModeInitialOracle {
		solveScript, err = readCandidateFile(taskRoot, SolveScriptRelativePath)
		if err != nil {
			return Candidate{}, err
		}
		testScript, err = readCandidateFile(taskRoot, TestScriptRelativePath)
		if err != nil {
			return Candidate{}, err
		}
	}
	return CandidateFromBytes(mode, dockerfile, solveScript, testScript)
}

// CandidateFromBytes applies the same digest contract to already frozen
// artifact bytes. Downstream admission and materialization use it to prove
// that a passing harness report names the exact artifacts they consume.
func CandidateFromBytes(mode Mode, dockerfile, solveScript, testScript []byte) (Candidate, error) {
	if err := mode.Validate(); err != nil {
		return Candidate{}, err
	}
	for name, content := range map[string][]byte{DockerfileRelativePath: dockerfile} {
		if len(content) == 0 || int64(len(content)) > taskpolicy.ManagedSnapshotFileMaxBytes {
			return Candidate{}, fmt.Errorf("Standard authoring harness candidate %s has invalid size", name)
		}
	}
	if mode == ModeInitialOracle {
		for name, content := range map[string][]byte{SolveScriptRelativePath: solveScript, TestScriptRelativePath: testScript} {
			if len(content) == 0 || int64(len(content)) > taskpolicy.ManagedSnapshotFileMaxBytes {
				return Candidate{}, fmt.Errorf("Standard authoring harness candidate %s has invalid size", name)
			}
		}
	} else if len(solveScript) != 0 || len(testScript) != 0 {
		return Candidate{}, errors.New("Dockerfile-only harness candidate contains solution or test bytes")
	}
	candidate := Candidate{
		Mode: mode, Dockerfile: append([]byte(nil), dockerfile...),
		SolveScript: append([]byte(nil), solveScript...), TestScript: append([]byte(nil), testScript...),
	}
	var err error
	candidate.EnvironmentDigest, err = workflowkit.FingerprintParts("harbor.standard-authoring.docker-environment.v1", []workflowkit.FingerprintPart{
		{Name: DockerfileRelativePath, Value: candidate.Dockerfile},
	})
	if err != nil {
		return Candidate{}, fmt.Errorf("fingerprint Standard authoring Docker environment: %w", err)
	}
	parts := []workflowkit.FingerprintPart{
		{Name: "mode", Value: []byte(mode)},
		{Name: DockerfileRelativePath, Value: candidate.Dockerfile},
	}
	if mode == ModeInitialOracle {
		parts = append(parts,
			workflowkit.FingerprintPart{Name: SolveScriptRelativePath, Value: candidate.SolveScript},
			workflowkit.FingerprintPart{Name: TestScriptRelativePath, Value: candidate.TestScript},
		)
	}
	candidate.CandidateDigest, err = workflowkit.FingerprintParts("harbor.standard-authoring.harness-candidate.v1", parts)
	if err != nil {
		return Candidate{}, fmt.Errorf("fingerprint Standard authoring harness candidate: %w", err)
	}
	return candidate, nil
}

func readCandidateFile(taskRoot, relative string) ([]byte, error) {
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
	if clean != relative {
		return nil, errors.New("Standard authoring harness candidate path is invalid")
	}
	directory := filepath.Join(taskRoot, filepath.Dir(filepath.FromSlash(relative)))
	if err := inspectCandidateDirectory(directory); err != nil {
		return nil, fmt.Errorf("inspect Standard authoring harness candidate directory %s: %w", filepath.Dir(relative), err)
	}
	path := filepath.Join(taskRoot, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > taskpolicy.ManagedSnapshotFileMaxBytes || candidateHasMultipleLinks(info) {
		return nil, fmt.Errorf("Standard authoring harness candidate file %s is unavailable or unsafe", relative)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Standard authoring harness candidate file %s: %w", relative, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || candidateHasMultipleLinks(opened) {
		return nil, fmt.Errorf("Standard authoring harness candidate file %s changed while opening", relative)
	}
	content, err := io.ReadAll(io.LimitReader(file, taskpolicy.ManagedSnapshotFileMaxBytes+1))
	if err != nil || int64(len(content)) != info.Size() {
		return nil, fmt.Errorf("Standard authoring harness candidate file %s changed while reading", relative)
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != int64(len(content)) || candidateHasMultipleLinks(after) {
		return nil, fmt.Errorf("Standard authoring harness candidate file %s changed while reading", relative)
	}
	return content, nil
}

func inspectCandidateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("candidate directory is unavailable or unsafe")
	}
	return nil
}

func candidateHasMultipleLinks(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink != 1
}
