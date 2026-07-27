package app

import (
	"testing"
)

func TestParseStandardAuthoringVerificationContractRejectsCoverageDowngradeAndPathEscapes(t *testing.T) {
	valid := []byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["wasm-pack","test","--headless","--firefox"],"workdir":".","coverage_mode":"browser_wasm","allowed_solution_paths":["src/app.rs","src/lib.rs"]}`)
	contract, err := ParseStandardAuthoringVerificationContractJSON(valid)
	if err != nil {
		t.Fatalf("parse valid contract: %v", err)
	}
	if contract.CoverageMode != StandardAuthoringCoverageBrowserWASM {
		t.Fatalf("coverage mode = %q", contract.CoverageMode)
	}

	for _, raw := range [][]byte{
		[]byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["cargo","test","--lib"],"workdir":".","coverage_mode":"browser_wasm","allowed_solution_paths":["src/lib.rs"]}`),
		[]byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["go","test","./..."],"workdir":"../outside","coverage_mode":"native","allowed_solution_paths":["src/main.go"]}`),
		[]byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["go","test","./..."],"workdir":".","coverage_mode":"native","allowed_solution_paths":["../outside"]}`),
		[]byte(`{"format":"harbor.verification-contract.v1","version":"1","command":["go","test","./..."],"workdir":".","coverage_mode":"native","allowed_solution_paths":["src/main.go"],"unexpected":true}`),
	} {
		if _, err := ParseStandardAuthoringVerificationContractJSON(raw); err == nil {
			t.Fatalf("invalid verification contract accepted: %s", raw)
		}
	}
}
