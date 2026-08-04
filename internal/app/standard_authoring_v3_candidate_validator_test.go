package app

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestStandardAuthoringV3FailureCodeSparesHostStepsFromRepairBudget(t *testing.T) {
	tests := []struct {
		name      string
		steps     []authoringharness.StepResult
		wantCode  workflowkit.AgentFailureCode
		wantSpare bool
	}{
		{
			name:      "environment build failure",
			steps:     []authoringharness.StepResult{{Step: "layout_probe", Passed: true}, {Step: "environment_build", Passed: false}},
			wantCode:  workflowkit.AgentFailureEnvironmentFault,
			wantSpare: true,
		},
		{
			name:      "source access failure",
			steps:     []authoringharness.StepResult{{Step: "layout_probe", Passed: true}, {Step: "environment_build", Passed: true}, {Step: "source_access", Passed: false}},
			wantCode:  workflowkit.AgentFailureEnvironmentFault,
			wantSpare: true,
		},
		{
			name:      "candidate defect stays validator reject",
			steps:     []authoringharness.StepResult{{Step: "layout_probe", Passed: true}, {Step: "environment_build", Passed: true}, {Step: "source_access", Passed: true}, {Step: "baseline_verify", Passed: false}},
			wantCode:  workflowkit.AgentFailureValidatorReject,
			wantSpare: false,
		},
		{
			name:      "all steps passed defaults to validator reject",
			steps:     []authoringharness.StepResult{{Step: "layout_probe", Passed: true}},
			wantCode:  workflowkit.AgentFailureValidatorReject,
			wantSpare: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := authoringharness.Result{Steps: test.steps}
			code := standardAuthoringV3FailureCode(result)
			if code != test.wantCode {
				t.Fatalf("failure code = %q, want %q", code, test.wantCode)
			}
			if consumed := code.ConsumesCandidateRepairBudget(); consumed == test.wantSpare {
				t.Fatalf("failure code %q repair budget consumption = %t, want spare:%t", code, !consumed, test.wantSpare)
			}
		})
	}
}
