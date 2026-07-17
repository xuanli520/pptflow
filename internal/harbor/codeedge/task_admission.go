package codeedge

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// TaskAdmissionContract is the immutable consumer policy an upstream producer
// must satisfy. It is deliberately a value: callers freeze its canonical bytes
// with their own deployment/run manifest instead of looking up a live profile.
type TaskAdmissionContract struct {
	ID      string  `json:"id"`
	Version string  `json:"version"`
	Profile Profile `json:"profile"`
}

// Validate rejects incomplete or malformed consumer contracts before they can
// be bound to an authoring execution.
func (contract TaskAdmissionContract) Validate() error {
	if strings.TrimSpace(contract.ID) == "" || strings.TrimSpace(contract.Version) == "" {
		return fmt.Errorf("CodeEdge task admission contract identity is required")
	}
	if err := ValidateProfile(contract.Profile); err != nil {
		return fmt.Errorf("validate CodeEdge task admission contract profile: %w", err)
	}
	return nil
}

// CanonicalJSON is the stable representation that callers bind into frozen
// manifests and receipts.
func (contract TaskAdmissionContract) CanonicalJSON() ([]byte, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return nil, fmt.Errorf("encode CodeEdge task admission contract: %w", err)
	}
	return encoded, nil
}

// Fingerprint identifies the exact contract, including its preflight profile.
func (contract TaskAdmissionContract) Fingerprint() (workflowkit.Fingerprint, error) {
	encoded, err := contract.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(encoded), nil
}

// TaskPackageFile is one canonical managed-task file. Paths are deliberately
// task-relative and are checked before any staging write occurs.
type TaskPackageFile struct {
	Path string
	Mode os.FileMode
	Data []byte
}

// AdmissionReport is a serializable deterministic admission result. A failed
// admission is a content result, not an infrastructure error.
type AdmissionReport struct {
	ContractID          string                  `json:"contract_id"`
	ContractVersion     string                  `json:"contract_version"`
	ContractFingerprint workflowkit.Fingerprint `json:"contract_fingerprint"`
	Passed              bool                    `json:"passed"`
	Violations          []Violation             `json:"violations"`
}

// ValidateTaskPackage applies the same preflight implementation used by the
// CodeEdge consumer to in-memory canonical package files. Its temporary
// directory is private and never returned to callers.
func ValidateTaskPackage(contract TaskAdmissionContract, files []TaskPackageFile) (AdmissionReport, error) {
	if err := contract.Validate(); err != nil {
		return AdmissionReport{}, err
	}
	fingerprint, err := contract.Fingerprint()
	if err != nil {
		return AdmissionReport{}, err
	}
	report := AdmissionReport{ContractID: contract.ID, ContractVersion: contract.Version, ContractFingerprint: fingerprint}
	root, err := os.MkdirTemp("", "harbor-codeedge-admission-*")
	if err != nil {
		return AdmissionReport{}, fmt.Errorf("create CodeEdge admission staging directory: %w", err)
	}
	defer os.RemoveAll(root)
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		clean, valid := canonicalTaskPackagePath(file.Path)
		if !valid {
			return AdmissionReport{}, fmt.Errorf("invalid CodeEdge task package path %q", file.Path)
		}
		if _, duplicate := seen[clean]; duplicate {
			return AdmissionReport{}, fmt.Errorf("duplicate CodeEdge task package path %q", clean)
		}
		seen[clean] = struct{}{}
		if len(file.Data) == 0 {
			return AdmissionReport{}, fmt.Errorf("CodeEdge task package file %q is empty", clean)
		}
		mode := file.Mode.Perm()
		if mode == 0 {
			return AdmissionReport{}, fmt.Errorf("CodeEdge task package file %q has no mode", clean)
		}
		path := filepath.Join(root, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return AdmissionReport{}, fmt.Errorf("create CodeEdge task package parent: %w", err)
		}
		if err := os.WriteFile(path, file.Data, mode); err != nil {
			return AdmissionReport{}, fmt.Errorf("write CodeEdge task package staging file: %w", err)
		}
	}
	_, err = InspectPhase1Task(root, contract.Profile)
	if err == nil {
		report.Passed = true
		return report, nil
	}
	validation, ok := err.(*ValidationError)
	if !ok {
		return AdmissionReport{}, fmt.Errorf("inspect CodeEdge task package: %w", err)
	}
	report.Violations = append([]Violation(nil), validation.Violations...)
	sort.Slice(report.Violations, func(i, j int) bool { return report.Violations[i].String() < report.Violations[j].String() })
	return report, nil
}

func canonicalTaskPackagePath(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return "", false
	}
	clean := path.Clean(value)
	return clean, clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
