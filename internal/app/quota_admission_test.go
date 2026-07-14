package app

import (
	"errors"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestBuildFrozenStageQuotaAdmissionUsesOnlyDescriptorClaims(t *testing.T) {
	template := workflowadapter.StandardWorkflowTemplate()
	resolved, err := template.Compile(lifecycleCompleteProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	stage, present := resolved.Descriptor.Stage(workflowkit.StageKey(workflowadapter.HarborRunQwen))
	if !present {
		t.Fatal("Qwen stage is absent")
	}
	admission, err := BuildFrozenStageQuotaAdmission(resolved.QuotaPolicy, stage)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Policy.PolicyID != workflowadapter.StandardQuotaPolicyID || admission.Policy.PolicyVersion != workflowadapter.StandardQuotaPolicyVersion || admission.Policy.PolicyFingerprint != string(resolved.QuotaPolicy.Fingerprint) {
		t.Fatalf("policy binding = %+v", admission.Policy)
	}
	if len(admission.BootstrapAccounts) != 4 || len(admission.Claims) != 2 {
		t.Fatalf("admission facts = %+v", admission)
	}
	if !containsStoreQuotaClaim(admission.Claims, "stage_attempt", 1) || !containsStoreQuotaClaim(admission.Claims, "trial", 4) {
		t.Fatalf("Qwen admission claims = %+v", admission.Claims)
	}

	tampered := stage.Clone()
	tampered.QuotaClaims[0].Units++
	if _, err := BuildFrozenStageQuotaAdmission(resolved.QuotaPolicy, tampered); !errors.Is(err, ErrFrozenQuotaAdmission) {
		t.Fatalf("tampered descriptor admission = %v, want ErrFrozenQuotaAdmission", err)
	}
}

func containsStoreQuotaClaim(claims []store.TaskActorQuotaClaim, dimension string, units int64) bool {
	for _, claim := range claims {
		if claim.Dimension == dimension && claim.Units == units {
			return true
		}
	}
	return false
}
