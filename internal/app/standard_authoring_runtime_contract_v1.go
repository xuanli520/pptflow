package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	StandardAuthoringRuntimeContractV1Format  = "harbor.standard-authoring-runtime-contract.v1"
	StandardAuthoringRuntimeContractV1Version = "1"

	standardAuthoringOracleRoot      = "/oracle"
	standardAuthoringOracleSource    = "/oracle/source"
	standardAuthoringOracleWorkspace = "/oracle/workspace"
)

// StandardAuthoringRuntimePathVariable is one of the only path variables a
// Standard Authoring container may receive. Values are part of the frozen
// contract, not environment discovery from the host process.
type StandardAuthoringRuntimePathVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// StandardAuthoringRuntimeContractV1 fixes the controlled authoring
// filesystem projection. Source is read-only and workspace is a fresh,
// stage-scoped writable worktree; only the host binds either path.
type StandardAuthoringRuntimeContractV1 struct {
	Format        string                                 `json:"format"`
	Version       string                                 `json:"version"`
	TaskRoot      string                                 `json:"task_root"`
	SourceRoot    string                                 `json:"source_root"`
	WorkspaceRoot string                                 `json:"workspace_root"`
	PathVariables []StandardAuthoringRuntimePathVariable `json:"path_variables"`
	Fingerprint   workflowkit.Fingerprint                `json:"fingerprint"`
}

// NewStandardAuthoringRuntimeContractV1 creates the sole accepted filesystem
// contract. There are no caller-selected path, mount, or environment fields.
func NewStandardAuthoringRuntimeContractV1() (StandardAuthoringRuntimeContractV1, error) {
	contract := StandardAuthoringRuntimeContractV1{
		Format: StandardAuthoringRuntimeContractV1Format, Version: StandardAuthoringRuntimeContractV1Version,
		TaskRoot: standardAuthoringOracleRoot, SourceRoot: standardAuthoringOracleSource, WorkspaceRoot: standardAuthoringOracleWorkspace,
		PathVariables: []StandardAuthoringRuntimePathVariable{
			{Name: "HARBOR_TASK_ROOT", Value: standardAuthoringOracleRoot},
			{Name: "HARBOR_SOURCE", Value: standardAuthoringOracleSource},
			{Name: "HARBOR_WORKSPACE", Value: standardAuthoringOracleWorkspace},
		},
	}
	fingerprint, err := standardAuthoringRuntimeContractV1Fingerprint(contract)
	if err != nil {
		return StandardAuthoringRuntimeContractV1{}, err
	}
	contract.Fingerprint = fingerprint
	if err := contract.Validate(); err != nil {
		return StandardAuthoringRuntimeContractV1{}, err
	}
	return contract, nil
}

// Clone returns an independently owned contract.
func (contract StandardAuthoringRuntimeContractV1) Clone() StandardAuthoringRuntimeContractV1 {
	contract.PathVariables = append([]StandardAuthoringRuntimePathVariable(nil), contract.PathVariables...)
	return contract
}

// Validate rejects an altered mount, an unapproved HARBOR_* variable, or a
// stale fingerprint before a candidate snapshot reaches a container.
func (contract StandardAuthoringRuntimeContractV1) Validate() error {
	if contract.Format != StandardAuthoringRuntimeContractV1Format || contract.Version != StandardAuthoringRuntimeContractV1Version {
		return fmt.Errorf("invalid Standard authoring runtime contract identity")
	}
	if contract.TaskRoot != standardAuthoringOracleRoot || contract.SourceRoot != standardAuthoringOracleSource || contract.WorkspaceRoot != standardAuthoringOracleWorkspace {
		return fmt.Errorf("invalid Standard authoring runtime mount layout")
	}
	expected := map[string]string{
		"HARBOR_TASK_ROOT": standardAuthoringOracleRoot,
		"HARBOR_SOURCE":    standardAuthoringOracleSource,
		"HARBOR_WORKSPACE": standardAuthoringOracleWorkspace,
	}
	if len(contract.PathVariables) != len(expected) {
		return fmt.Errorf("invalid Standard authoring runtime path variable count")
	}
	for _, variable := range contract.PathVariables {
		if expected[variable.Name] != variable.Value {
			return fmt.Errorf("invalid Standard authoring runtime path variable %q", variable.Name)
		}
		delete(expected, variable.Name)
	}
	if len(expected) != 0 {
		return fmt.Errorf("incomplete Standard authoring runtime path variables")
	}
	if err := contract.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("invalid Standard authoring runtime contract fingerprint: %w", err)
	}
	want, err := standardAuthoringRuntimeContractV1Fingerprint(contract)
	if err != nil {
		return err
	}
	if contract.Fingerprint != want {
		return fmt.Errorf("Standard authoring runtime contract fingerprint does not match content")
	}
	return nil
}

func standardAuthoringRuntimeContractV1Fingerprint(contract StandardAuthoringRuntimeContractV1) (workflowkit.Fingerprint, error) {
	canonical := contract.Clone()
	canonical.Fingerprint = ""
	sort.Slice(canonical.PathVariables, func(left, right int) bool {
		return canonical.PathVariables[left].Name < canonical.PathVariables[right].Name
	})
	for _, variable := range canonical.PathVariables {
		if strings.TrimSpace(variable.Name) == "" || strings.TrimSpace(variable.Value) == "" {
			return "", fmt.Errorf("invalid Standard authoring runtime contract path variable")
		}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode Standard authoring runtime contract: %w", err)
	}
	return workflowkit.FingerprintBytes("harbor.standard-authoring-runtime-contract.v1", encoded)
}
