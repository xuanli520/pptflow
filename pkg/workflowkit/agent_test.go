package workflowkit

import (
	"errors"
	"testing"
)

func TestAgentRoleSpecValidatesClosedAuthorities(t *testing.T) {
	tests := []struct {
		name string
		spec AgentRoleSpec
	}{
		{
			name: "researcher evidence from read-only snapshot",
			spec: testAgentRoleSpec(AgentRoleResearcher, AgentOutputEvidence, WorkspaceReadOnlySnapshot),
		},
		{
			name: "synthesizer structured artifact from read-only snapshot",
			spec: testAgentRoleSpec(AgentRoleSynthesizer, AgentOutputStructuredArtifact, WorkspaceReadOnlySnapshot),
		},
		{
			name: "author candidate from exclusive workspace",
			spec: testAgentRoleSpec(AgentRoleAuthor, AgentOutputCandidateSnapshot, WorkspaceExclusiveWriter),
		},
		{
			name: "critic finding from read-only snapshot",
			spec: testAgentRoleSpec(AgentRoleCritic, AgentOutputFinding, WorkspaceReadOnlySnapshot),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.spec.Validate(); err != nil {
				t.Fatalf("validate role specification: %v", err)
			}
		})
	}
}

func TestAgentRoleSpecRejectsAuthorityEscalation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AgentRoleSpec)
	}{
		{
			name:   "unknown role",
			mutate: func(spec *AgentRoleSpec) { spec.RoleID = "operator" },
		},
		{
			name:   "author non-candidate output",
			mutate: func(spec *AgentRoleSpec) { spec.OutputMode = AgentOutputStructuredArtifact },
		},
		{
			name:   "author no validation allowance",
			mutate: func(spec *AgentRoleSpec) { spec.MaxValidationAttempts = 0 },
		},
		{
			name: "critic writer workspace",
			mutate: func(spec *AgentRoleSpec) {
				spec.RoleID = AgentRoleCritic
				spec.OutputMode = AgentOutputFinding
				spec.Workspace.Mode = WorkspaceExclusiveWriter
			},
		},
		{
			name: "researcher finding output",
			mutate: func(spec *AgentRoleSpec) {
				spec.RoleID = AgentRoleResearcher
				spec.OutputMode = AgentOutputFinding
				spec.Workspace.Mode = WorkspaceReadOnlySnapshot
			},
		},
		{
			name: "tool path",
			mutate: func(spec *AgentRoleSpec) {
				spec.AllowedDynamicTools = []string{"harbor_validate_candidate", "../../read"}
			},
		},
		{
			name:   "workspace snapshot is not an input",
			mutate: func(spec *AgentRoleSpec) { spec.Workspace.SnapshotArtifact = "missing" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := testAgentRoleSpec(AgentRoleAuthor, AgentOutputCandidateSnapshot, WorkspaceExclusiveWriter)
			test.mutate(&spec)
			if err := spec.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
				t.Fatalf("validate error = %v, want ErrInvalidDescriptor", err)
			}
		})
	}
}

func TestAgentRoleSpecCloneAndFingerprintAreCanonical(t *testing.T) {
	left := testAgentRoleSpec(AgentRoleAuthor, AgentOutputCandidateSnapshot, WorkspaceExclusiveWriter)
	left.InputSchemas = append(left.InputSchemas, ArtifactSpec{Name: "review_input", SchemaVersion: "review.v1", Required: false})
	left.AllowedDynamicTools = []string{"harbor_validate_candidate", "harbor_submit_candidate"}

	right := left.Clone()
	right.InputSchemas[0], right.InputSchemas[1] = right.InputSchemas[1], right.InputSchemas[0]
	right.AllowedDynamicTools[0], right.AllowedDynamicTools[1] = right.AllowedDynamicTools[1], right.AllowedDynamicTools[0]
	leftFingerprint, err := left.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint left role specification: %v", err)
	}
	rightFingerprint, err := right.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint right role specification: %v", err)
	}
	if leftFingerprint != rightFingerprint {
		t.Fatalf("canonical fingerprints differ: %s != %s", leftFingerprint, rightFingerprint)
	}

	clone := left.Clone()
	clone.InputSchemas[0].Name = "mutated"
	clone.AllowedDynamicTools[0] = "mutated_tool"
	if left.InputSchemas[0].Name == clone.InputSchemas[0].Name || left.AllowedDynamicTools[0] == clone.AllowedDynamicTools[0] {
		t.Fatalf("clone aliases mutable role fields: original=%#v clone=%#v", left, clone)
	}
}

func TestStageDescriptorBindsAgentRoleToFrozenWorkspace(t *testing.T) {
	stage := snapshotWorkspaceStage("author", WorkspaceExclusiveWriter, []ResourceKey{"candidate/output"})
	role := testAgentRoleSpec(AgentRoleAuthor, AgentOutputCandidateSnapshot, WorkspaceExclusiveWriter)
	role.Workspace.Key = stage.Concurrency.Workspace.Key
	role.Workspace.SnapshotArtifact = stage.Concurrency.Workspace.SnapshotArtifact
	role.InputSchemas[0].Name = stage.Concurrency.Workspace.SnapshotArtifact
	role.InputSchemas[0].SchemaVersion = stage.Inputs[0].SchemaVersion
	stage.AgentRole = &role
	if err := stage.Validate(); err != nil {
		t.Fatalf("validate author stage role binding: %v", err)
	}

	mismatched := stage.Clone()
	mismatched.AgentRole.Workspace.Key = "candidate/other"
	if err := mismatched.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("mismatched agent workspace error = %v, want ErrInvalidDescriptor", err)
	}

	unknownInput := stage.Clone()
	unknownInput.AgentRole.InputSchemas[0].Name = "other_snapshot"
	if err := unknownInput.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("undeclared agent input error = %v, want ErrInvalidDescriptor", err)
	}
}

func TestAgentAttemptReportLimitsDurableObservability(t *testing.T) {
	report, err := NewAgentAttemptReport(AgentAttemptReport{
		RoleID:          AgentRoleAuthor,
		InputDigest:     SHA256Fingerprint([]byte("inputs")),
		CandidateDigest: SHA256Fingerprint([]byte("candidate")),
		ContractDigest:  SHA256Fingerprint([]byte("contract")),
		Turns:           2,
		Commands: []AgentCommandReport{{
			CommandID: "verification", ExitCode: 1, TestStarted: true, StdoutTail: "[redacted]",
		}},
	})
	if err != nil {
		t.Fatalf("create agent report: %v", err)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("validate agent report: %v", err)
	}
	if AgentFailureEnvironmentFault.ConsumesCandidateRepairBudget() {
		t.Fatal("environment fault consumed an author repair attempt")
	}
	if !AgentFailureValidatorReject.ConsumesCandidateRepairBudget() {
		t.Fatal("validator rejection did not consume a candidate repair attempt")
	}

	missingCandidate := report
	missingCandidate.CandidateDigest = ""
	if err := missingCandidate.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("successful author report without candidate error = %v, want ErrInvalidDescriptor", err)
	}
	oversized := report
	oversized.Commands[0].StderrTail = string(make([]byte, MaxRedactedDiagnosticTail+1))
	if err := oversized.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("oversized diagnostics error = %v, want ErrInvalidDescriptor", err)
	}
}

func testAgentRoleSpec(role AgentRoleID, output AgentOutputMode, workspaceMode WorkspaceMode) AgentRoleSpec {
	return AgentRoleSpec{
		RoleID:                 role,
		PromptAssetFingerprint: SHA256Fingerprint([]byte("prompt-" + string(role))),
		InputSchemas: []ArtifactSpec{{
			Name: "snapshot", SchemaVersion: "workflow.snapshot.v1", Required: true,
		}},
		Workspace: WorkspaceBinding{
			Mode: workspaceMode, Key: "candidate", SnapshotArtifact: "snapshot",
		},
		AllowedDynamicTools:   []string{"harbor_validate_candidate"},
		OutputMode:            output,
		MaxTurns:              3,
		MaxValidationAttempts: 1,
	}
}
