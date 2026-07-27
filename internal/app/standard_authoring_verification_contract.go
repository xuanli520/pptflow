package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	StandardAuthoringVerificationContractFormat  = "harbor.verification-contract.v1"
	StandardAuthoringVerificationContractVersion = "1"
)

// StandardAuthoringCoverageMode freezes the coverage surface a candidate must
// exercise. It is deliberately not inferred from a generated test script.
type StandardAuthoringCoverageMode string

const (
	StandardAuthoringCoverageNative      StandardAuthoringCoverageMode = "native"
	StandardAuthoringCoverageIntegration StandardAuthoringCoverageMode = "integration"
	StandardAuthoringCoverageBrowserWASM StandardAuthoringCoverageMode = "browser_wasm"
)

// StandardAuthoringVerificationContract is the human-approved, typed command
// contract emitted by task_synthesis and frozen by task_review. The command is
// parsed and executed only by the host verifier; it is never a dynamic tool
// argument and cannot be replaced by an author-provided shell program.
type StandardAuthoringVerificationContract struct {
	Format               string                        `json:"format"`
	Version              string                        `json:"version"`
	Command              []string                      `json:"command"`
	Workdir              string                        `json:"workdir"`
	CoverageMode         StandardAuthoringCoverageMode `json:"coverage_mode"`
	AllowedSolutionPaths []string                      `json:"allowed_solution_paths"`
}

// ParseStandardAuthoringVerificationContractJSON accepts only canonical,
// duplicate-free v1 contracts. Canonical bytes are part of the frozen artifact
// identity, preventing semantically equal but unreviewed documents from being
// silently accepted after task_review.
func ParseStandardAuthoringVerificationContractJSON(raw []byte) (StandardAuthoringVerificationContract, error) {
	if len(raw) == 0 || len(raw) > 64<<10 || !json.Valid(raw) {
		return StandardAuthoringVerificationContract{}, fmt.Errorf("Standard authoring verification contract is invalid")
	}
	if err := rejectDuplicateStandardAuthoringJSONKeys(raw); err != nil {
		return StandardAuthoringVerificationContract{}, fmt.Errorf("Standard authoring verification contract: %w", err)
	}
	var contract StandardAuthoringVerificationContract
	if err := decodeStrictJSON(string(raw), &contract); err != nil {
		return StandardAuthoringVerificationContract{}, fmt.Errorf("Standard authoring verification contract: %w", err)
	}
	if err := contract.Validate(); err != nil {
		return StandardAuthoringVerificationContract{}, err
	}
	canonical, err := json.Marshal(contract.Canonical())
	if err != nil || !bytes.Equal(raw, canonical) {
		return StandardAuthoringVerificationContract{}, fmt.Errorf("Standard authoring verification contract is not canonical")
	}
	return contract.Canonical(), nil
}

func (contract StandardAuthoringVerificationContract) Clone() StandardAuthoringVerificationContract {
	contract.Command = append([]string(nil), contract.Command...)
	contract.AllowedSolutionPaths = append([]string(nil), contract.AllowedSolutionPaths...)
	return contract
}

// Canonical returns the deterministic representation after validation. The
// command itself remains ordered; solution paths are a set.
func (contract StandardAuthoringVerificationContract) Canonical() StandardAuthoringVerificationContract {
	contract = contract.Clone()
	sort.Strings(contract.AllowedSolutionPaths)
	return contract
}

func (contract StandardAuthoringVerificationContract) Digest() (workflowkit.Fingerprint, error) {
	if err := contract.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(contract.Canonical())
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(raw), nil
}

func (contract StandardAuthoringVerificationContract) Validate() error {
	if contract.Format != StandardAuthoringVerificationContractFormat || contract.Version != StandardAuthoringVerificationContractVersion {
		return fmt.Errorf("Standard authoring verification contract identity is invalid")
	}
	if len(contract.Command) == 0 || len(contract.Command) > 32 {
		return fmt.Errorf("Standard authoring verification contract command is invalid")
	}
	for _, argument := range contract.Command {
		if strings.TrimSpace(argument) == "" || len(argument) > 1024 || strings.IndexByte(argument, '\x00') >= 0 || strings.ContainsAny(argument, "\r\n") {
			return fmt.Errorf("Standard authoring verification contract command is invalid")
		}
	}
	if !standardAuthoringVerificationRelativePath(contract.Workdir, true) {
		return fmt.Errorf("Standard authoring verification contract workdir is invalid")
	}
	switch contract.CoverageMode {
	case StandardAuthoringCoverageNative, StandardAuthoringCoverageIntegration, StandardAuthoringCoverageBrowserWASM:
	default:
		return fmt.Errorf("Standard authoring verification contract coverage mode is invalid")
	}
	if len(contract.AllowedSolutionPaths) == 0 || len(contract.AllowedSolutionPaths) > 256 {
		return fmt.Errorf("Standard authoring verification contract allowed solution paths are invalid")
	}
	previous := ""
	for _, candidate := range contract.AllowedSolutionPaths {
		if !standardAuthoringVerificationRelativePath(candidate, false) || candidate <= previous {
			return fmt.Errorf("Standard authoring verification contract allowed solution paths are invalid")
		}
		previous = candidate
	}
	if contract.CoverageMode == StandardAuthoringCoverageBrowserWASM && !standardAuthoringBrowserWASMCommand(contract.Command) {
		return fmt.Errorf("Standard authoring browser_wasm coverage cannot be downgraded to a non-browser command")
	}
	return nil
}

func standardAuthoringVerificationRelativePath(value string, allowRoot bool) bool {
	if value == "." && allowRoot {
		return true
	}
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\\") || strings.IndexByte(value, '\x00') >= 0 || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func standardAuthoringBrowserWASMCommand(command []string) bool {
	joined := strings.ToLower(strings.Join(command, " "))
	if strings.Contains(joined, "cargo test --lib") || strings.Contains(joined, "cargo test --bins") {
		return false
	}
	return strings.Contains(joined, "wasm-bindgen-test") || strings.Contains(joined, "wasm-pack test") || strings.Contains(joined, "trunk test") || strings.Contains(joined, "playwright") || strings.Contains(joined, "browser") || strings.Contains(joined, "wasm32")
}
