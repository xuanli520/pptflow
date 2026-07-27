package app

import "testing"

func TestStandardAuthoringRuntimeContractV1IsClosedAndFingerprinted(t *testing.T) {
	contract, err := NewStandardAuthoringRuntimeContractV1()
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("validate runtime contract: %v", err)
	}
	if contract.TaskRoot != "/oracle" || contract.SourceRoot != "/oracle/source" || contract.WorkspaceRoot != "/oracle/workspace" {
		t.Fatalf("runtime paths = %+v", contract)
	}

	mutated := contract.Clone()
	mutated.PathVariables[0].Value = "/tmp/model-controlled"
	if err := mutated.Validate(); err == nil {
		t.Fatal("runtime contract accepted a model-controlled path variable")
	}
	missing := contract.Clone()
	missing.PathVariables = missing.PathVariables[:2]
	if err := missing.Validate(); err == nil {
		t.Fatal("runtime contract accepted an incomplete path variable allowlist")
	}
}
