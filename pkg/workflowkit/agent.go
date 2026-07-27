package workflowkit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	AgentAttemptReportFormat  = "workflowkit.agent-attempt-report.v1"
	AgentAttemptReportVersion = "1"
	MaxRedactedDiagnosticTail = 4096
)

// AgentRoleID identifies one of the closed, deployment-defined agent roles.
// A role never carries free-form instructions or a model-selected workspace
// path; those authorities remain in frozen catalog assets and bindings.
type AgentRoleID string

const (
	AgentRoleResearcher  AgentRoleID = "researcher"
	AgentRoleSynthesizer AgentRoleID = "synthesizer"
	AgentRoleAuthor      AgentRoleID = "author"
	AgentRoleCritic      AgentRoleID = "critic"
)

func (role AgentRoleID) valid() bool {
	switch role {
	case AgentRoleResearcher, AgentRoleSynthesizer, AgentRoleAuthor, AgentRoleCritic:
		return true
	default:
		return false
	}
}

// AgentOutputMode describes the only authority an agent may exercise. Every
// mode produces a host-validated, typed artifact; free assistant text is never
// an authoritative workflow output.
type AgentOutputMode string

const (
	AgentOutputEvidence           AgentOutputMode = "evidence"
	AgentOutputFinding            AgentOutputMode = "finding"
	AgentOutputStructuredArtifact AgentOutputMode = "structured_artifact"
	AgentOutputCandidateSnapshot  AgentOutputMode = "candidate_snapshot"
)

func (mode AgentOutputMode) valid() bool {
	switch mode {
	case AgentOutputEvidence, AgentOutputFinding, AgentOutputStructuredArtifact, AgentOutputCandidateSnapshot:
		return true
	default:
		return false
	}
}

// AgentFailureCode is a stable, persisted-safe outcome classification for an
// agent attempt. It intentionally supplements transport-level FailureClass:
// retry policy can still reason about a network timeout without conflating it
// with a candidate or contract violation.
type AgentFailureCode string

const (
	AgentFailureNone                AgentFailureCode = ""
	AgentFailureCandidateInvalid    AgentFailureCode = "candidate_invalid"
	AgentFailureProtocol            AgentFailureCode = "agent_protocol"
	AgentFailureValidatorReject     AgentFailureCode = "validator_reject"
	AgentFailureWorkspaceContract   AgentFailureCode = "workspace_contract"
	AgentFailureEnvironmentFault    AgentFailureCode = "environment_fault"
	AgentFailureInfrastructureFault AgentFailureCode = "infrastructure_fault"
	AgentFailureQuotaExhausted      AgentFailureCode = "quota_exhausted"
)

func (code AgentFailureCode) valid() bool {
	switch code {
	case AgentFailureNone, AgentFailureCandidateInvalid, AgentFailureProtocol, AgentFailureValidatorReject,
		AgentFailureWorkspaceContract, AgentFailureEnvironmentFault, AgentFailureInfrastructureFault,
		AgentFailureQuotaExhausted:
		return true
	default:
		return false
	}
}

// ConsumesCandidateRepairBudget reports whether the classified failure is a
// candidate defect that may be presented to a newly fenced author session.
// Host/environment/infrastructure failures never consume an agent repair
// opportunity.
func (code AgentFailureCode) ConsumesCandidateRepairBudget() bool {
	switch code {
	case AgentFailureCandidateInvalid, AgentFailureProtocol, AgentFailureValidatorReject, AgentFailureWorkspaceContract:
		return true
	default:
		return false
	}
}

// AgentCommandReport is the sole command-level observability surface allowed
// in a durable agent report. Callers must redact output before constructing
// it; raw tool payloads, transcripts, credentials, and environment values do
// not have fields in this contract.
type AgentCommandReport struct {
	CommandID   string `json:"command_id"`
	ExitCode    int    `json:"exit_code"`
	TestStarted bool   `json:"test_started"`
	StdoutTail  string `json:"stdout_tail,omitempty"`
	StderrTail  string `json:"stderr_tail,omitempty"`
}

func (report AgentCommandReport) validate() error {
	if err := validateRequired("agent command id", report.CommandID, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, tail := range []string{report.StdoutTail, report.StderrTail} {
		if len(tail) > MaxRedactedDiagnosticTail {
			return fmt.Errorf("%w: redacted command output exceeds %d bytes", ErrInvalidDescriptor, MaxRedactedDiagnosticTail)
		}
		if strings.IndexByte(tail, '\x00') >= 0 {
			return fmt.Errorf("%w: redacted command output contains a NUL byte", ErrInvalidDescriptor)
		}
	}
	return nil
}

// AgentAttemptReport is a compact, versioned observation of one agent
// attempt. It deliberately records immutable references and bounded,
// pre-redacted host results only; it is not a transcript archive.
type AgentAttemptReport struct {
	Format                  string               `json:"format"`
	Version                 string               `json:"version"`
	RoleID                  AgentRoleID          `json:"role_id"`
	InputDigest             Fingerprint          `json:"input_digest"`
	CandidateDigest         Fingerprint          `json:"candidate_digest,omitempty"`
	ContractDigest          Fingerprint          `json:"contract_digest"`
	Turns                   int                  `json:"turns"`
	ValidationReceiptDigest Fingerprint          `json:"validation_receipt_digest,omitempty"`
	FailureCode             AgentFailureCode     `json:"failure_code,omitempty"`
	Commands                []AgentCommandReport `json:"commands"`
}

// NewAgentAttemptReport returns a canonical, validated report with the fixed
// v1 identity. The report can be stored as an ordinary immutable artifact,
// avoiding a transcript table or a role-specific persistence schema.
func NewAgentAttemptReport(report AgentAttemptReport) (AgentAttemptReport, error) {
	report.Format = AgentAttemptReportFormat
	report.Version = AgentAttemptReportVersion
	report.Commands = append([]AgentCommandReport(nil), report.Commands...)
	if report.Commands == nil {
		report.Commands = []AgentCommandReport{}
	}
	if err := report.Validate(); err != nil {
		return AgentAttemptReport{}, err
	}
	return report, nil
}

// Clone returns an independently owned report value.
func (report AgentAttemptReport) Clone() AgentAttemptReport {
	report.Commands = append([]AgentCommandReport(nil), report.Commands...)
	return report
}

// Validate proves a report contains only bounded references and the closed
// observability fields permitted for durable storage.
func (report AgentAttemptReport) Validate() error {
	if report.Format != AgentAttemptReportFormat || report.Version != AgentAttemptReportVersion {
		return fmt.Errorf("%w: unsupported agent attempt report identity", ErrInvalidDescriptor)
	}
	if !report.RoleID.valid() {
		return fmt.Errorf("%w: unsupported agent report role %q", ErrInvalidDescriptor, report.RoleID)
	}
	for label, digest := range map[string]Fingerprint{"input": report.InputDigest, "contract": report.ContractDigest} {
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%w: agent report %s digest: %v", ErrInvalidDescriptor, label, err)
		}
	}
	for label, digest := range map[string]Fingerprint{"candidate": report.CandidateDigest, "validation receipt": report.ValidationReceiptDigest} {
		if digest == "" {
			continue
		}
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%w: agent report %s digest: %v", ErrInvalidDescriptor, label, err)
		}
	}
	if report.Turns < 0 {
		return fmt.Errorf("%w: agent report turns cannot be negative", ErrInvalidDescriptor)
	}
	if !report.FailureCode.valid() {
		return fmt.Errorf("%w: unsupported agent failure code %q", ErrInvalidDescriptor, report.FailureCode)
	}
	if report.RoleID == AgentRoleAuthor && report.CandidateDigest == "" && report.FailureCode == AgentFailureNone {
		return fmt.Errorf("%w: successful author report requires a candidate digest", ErrInvalidDescriptor)
	}
	if report.ValidationReceiptDigest != "" && report.CandidateDigest == "" {
		return fmt.Errorf("%w: validation receipt requires a candidate digest", ErrInvalidDescriptor)
	}
	commandIDs := make(map[string]struct{}, len(report.Commands))
	for _, command := range report.Commands {
		if err := command.validate(); err != nil {
			return err
		}
		if _, exists := commandIDs[command.CommandID]; exists {
			return fmt.Errorf("%w: duplicate agent command report %q", ErrInvalidDescriptor, command.CommandID)
		}
		commandIDs[command.CommandID] = struct{}{}
	}
	return nil
}

// AgentRoleSpec is the version-one frozen capability contract for one agent
// stage. It contains only typed input, workspace, output, tool, and budget
// declarations; runtimes must resolve the prompt asset by its fingerprint.
type AgentRoleSpec struct {
	RoleID                 AgentRoleID      `json:"role_id"`
	PromptAssetFingerprint Fingerprint      `json:"prompt_asset_fingerprint"`
	InputSchemas           []ArtifactSpec   `json:"input_schemas"`
	Workspace              WorkspaceBinding `json:"workspace"`
	AllowedDynamicTools    []string         `json:"allowed_dynamic_tools"`
	OutputMode             AgentOutputMode  `json:"output_mode"`
	MaxTurns               int              `json:"max_turns"`
	MaxValidationAttempts  int              `json:"max_validation_attempts"`
}

// Clone returns an independently owned role specification.
func (spec AgentRoleSpec) Clone() AgentRoleSpec {
	spec.InputSchemas = append([]ArtifactSpec(nil), spec.InputSchemas...)
	spec.Workspace = spec.Workspace.Clone()
	spec.AllowedDynamicTools = append([]string(nil), spec.AllowedDynamicTools...)
	return spec
}

func (spec AgentRoleSpec) canonical() AgentRoleSpec {
	spec = spec.Clone()
	spec.Workspace = spec.Workspace.canonical()
	sort.Slice(spec.InputSchemas, func(left, right int) bool {
		return spec.InputSchemas[left].Name < spec.InputSchemas[right].Name
	})
	sort.Strings(spec.AllowedDynamicTools)
	return spec
}

// Canonical returns a validated, canonical role specification. Catalog
// adapters use this before fingerprinting a frozen deployment asset.
func (spec AgentRoleSpec) Canonical() (AgentRoleSpec, error) {
	if err := spec.Validate(); err != nil {
		return AgentRoleSpec{}, err
	}
	return spec.canonical(), nil
}

// Validate verifies that a role is closed, its input/workspace contract is
// host-addressable, and it cannot gain a free-form output or write authority.
func (spec AgentRoleSpec) Validate() error {
	if !spec.RoleID.valid() {
		return fmt.Errorf("%w: unsupported agent role %q", ErrInvalidDescriptor, spec.RoleID)
	}
	if err := spec.PromptAssetFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: agent prompt asset fingerprint: %v", ErrInvalidDescriptor, err)
	}
	if err := validateArtifactSpecs("agent input", spec.InputSchemas); err != nil {
		return err
	}
	if err := spec.Workspace.validate(spec.InputSchemas); err != nil {
		return err
	}
	if err := validateAgentDynamicTools(spec.AllowedDynamicTools); err != nil {
		return err
	}
	if !spec.OutputMode.valid() {
		return fmt.Errorf("%w: unsupported agent output mode %q", ErrInvalidDescriptor, spec.OutputMode)
	}
	if spec.MaxTurns <= 0 {
		return fmt.Errorf("%w: agent maximum turns must be positive", ErrInvalidDescriptor)
	}
	if spec.MaxValidationAttempts < 0 {
		return fmt.Errorf("%w: agent maximum validation attempts cannot be negative", ErrInvalidDescriptor)
	}
	if err := validateAgentRoleAuthority(spec); err != nil {
		return err
	}
	return nil
}

func validateAgentDynamicTools(tools []string) error {
	if err := validateUniqueStrings("agent dynamic tool", tools, ErrInvalidDescriptor); err != nil {
		return err
	}
	for _, tool := range tools {
		if !validAgentDynamicToolName(tool) {
			return fmt.Errorf("%w: agent dynamic tool %q is not a closed tool identifier", ErrInvalidDescriptor, tool)
		}
	}
	return nil
}

func validAgentDynamicToolName(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for index := range value {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9' && index > 0) || (character == '_' && index > 0) {
			continue
		}
		return false
	}
	return true
}

func validateAgentRoleAuthority(spec AgentRoleSpec) error {
	switch spec.RoleID {
	case AgentRoleResearcher:
		if spec.OutputMode != AgentOutputEvidence {
			return fmt.Errorf("%w: researcher role may emit only evidence", ErrInvalidDescriptor)
		}
	case AgentRoleSynthesizer:
		if spec.OutputMode != AgentOutputStructuredArtifact {
			return fmt.Errorf("%w: synthesizer role must emit a structured artifact", ErrInvalidDescriptor)
		}
	case AgentRoleAuthor:
		if spec.OutputMode != AgentOutputCandidateSnapshot {
			return fmt.Errorf("%w: author role may emit only a candidate snapshot", ErrInvalidDescriptor)
		}
		if spec.Workspace.canonical().Mode != WorkspaceExclusiveWriter {
			return fmt.Errorf("%w: author role requires an exclusive writer workspace", ErrInvalidDescriptor)
		}
		if spec.MaxValidationAttempts == 0 {
			return fmt.Errorf("%w: author role requires at least one validation attempt", ErrInvalidDescriptor)
		}
	case AgentRoleCritic:
		if spec.OutputMode != AgentOutputEvidence && spec.OutputMode != AgentOutputFinding {
			return fmt.Errorf("%w: critic role may emit only evidence or a finding", ErrInvalidDescriptor)
		}
	}
	if spec.RoleID != AgentRoleAuthor && spec.Workspace.canonical().Mode == WorkspaceExclusiveWriter {
		return fmt.Errorf("%w: only the author role may receive an exclusive writer workspace", ErrInvalidDescriptor)
	}
	return nil
}

// Fingerprint returns a canonical, domain-separated identity for the frozen
// role contract. Input schemas and tool allowlists are sets, so declaration
// order does not affect the result.
func (spec AgentRoleSpec) Fingerprint() (Fingerprint, error) {
	if err := spec.Validate(); err != nil {
		return "", err
	}
	canonical := spec.canonical()
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode agent role specification: %v", ErrInvalidDescriptor, err)
	}
	return FingerprintBytes("workflowkit.agent-role-spec.v1", encoded)
}
