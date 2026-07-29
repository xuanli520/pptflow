package app

import "testing"

func TestStandardAuthoringRuntimeContractV1IsLegacyClosedAndFingerprinted(t *testing.T) {
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

func TestStandardAuthoringRuntimeContractV2IsCurrentValidationABI(t *testing.T) {
	contract, err := NewStandardAuthoringRuntimeContractV2()
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.Validate(); err != nil {
		t.Fatalf("validate current runtime contract: %v", err)
	}
	if contract.TaskRoot != "/task" || contract.SourceRoot != "/source" || contract.WorkspaceRoot != "/work" {
		t.Fatalf("current runtime paths = %+v", contract)
	}
	want := map[string]string{
		"HARBOR_TASK_ROOT": "/task",
		"HARBOR_SOURCE":    "/source",
		"HARBOR_WORKSPACE": "/work",
	}
	for _, variable := range contract.PathVariables {
		if want[variable.Name] != variable.Value {
			t.Fatalf("current runtime variable %q = %q", variable.Name, variable.Value)
		}
		delete(want, variable.Name)
	}
	if len(want) != 0 {
		t.Fatalf("current runtime variables missing: %+v", want)
	}
}
