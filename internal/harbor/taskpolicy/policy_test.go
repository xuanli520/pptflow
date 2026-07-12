package taskpolicy

import "testing"

func TestContainsLegacyDomainUsesIdentifierBoundaries(t *testing.T) {
	for _, value := range []string{
		"environment/promptflow_runner.py",
		"legacy image2 residue",
		"pptflow/config.json",
	} {
		if !ContainsLegacyDomain(value) {
			t.Errorf("expected legacy match for %q", value)
		}
	}

	for _, value := range []string{
		"encoded representation",
		"compressed transfer representation",
		"slider component",
		"image2d decoder",
		"presentationLayer",
		"PowerPoint presentation export",
		"slide deck parser",
	} {
		if ContainsLegacyDomain(value) {
			t.Errorf("unexpected legacy match for %q", value)
		}
	}
}

func TestIsAllowedFileUsesCanonicalHarborFileSet(t *testing.T) {
	for _, path := range []string{
		"instruction.md",
		"environment/Dockerfile",
		"environment/docker-compose.yaml",
		"solution/solve.sh",
		"tests/test.sh",
	} {
		if !IsAllowedFile(path) {
			t.Errorf("expected allowed file %q", path)
		}
	}
	if IsAllowedFile("environment/promptflow_runner.py") {
		t.Fatal("unexpected legacy file allowed")
	}
}
