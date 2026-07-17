package workflowadapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const standardAuthoringPolicyTestImage = "docker.io/library/rust:1.65.0-bullseye@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestStandardAuthoringEnvironmentPolicyCanonicalJSONAndContentDigest(t *testing.T) {
	policy, err := NewStandardAuthoringEnvironmentPolicy(standardAuthoringPolicyTestImage)
	if err != nil {
		t.Fatalf("create environment policy: %v", err)
	}
	canonical, err := policy.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonicalize environment policy: %v", err)
	}
	want := `{"format":"harbor.standard-authoring-environment-policy.v1","version":"1","base_image":"` + standardAuthoringPolicyTestImage + `"}`
	if string(canonical) != want {
		t.Fatalf("canonical policy = %s, want %s", canonical, want)
	}
	digest, err := policy.ContentDigest()
	if err != nil {
		t.Fatalf("environment policy digest: %v", err)
	}
	if digest != workflowkit.SHA256Fingerprint(canonical) {
		t.Fatalf("environment policy digest = %q, want plain canonical SHA-256", digest)
	}
	fingerprint, err := policy.Fingerprint()
	if err != nil || fingerprint != digest {
		t.Fatalf("environment policy fingerprint = %q, %v; want %q", fingerprint, err, digest)
	}

	parsed, err := ParseStandardAuthoringEnvironmentPolicyJSON(canonical)
	if err != nil || parsed != policy {
		t.Fatalf("parse canonical policy = %+v, %v; want %+v", parsed, err, policy)
	}
	var direct StandardAuthoringEnvironmentPolicy
	if err := json.Unmarshal(canonical, &direct); err != nil || direct != policy {
		t.Fatalf("direct strict policy decode = %+v, %v; want %+v", direct, err, policy)
	}
}

func TestStandardAuthoringEnvironmentPolicyRejectsMutableOrAmbiguousImages(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, image := range []string{
		"",
		"rust:1.65.0-bullseye@sha256:" + digest,
		"library/rust:1.65.0-bullseye@sha256:" + digest,
		"docker.io/library/rust:1.65.0-bullseye",
		"docker.io/library/rust:1.65.0-bullseye@sha256:" + strings.Repeat("A", 64),
		"docker.io/library/rust:1.65.0-bullseye@sha512:" + digest,
		"docker.io/library/${RUST}:1.65.0@sha256:" + digest,
		"docker.io/library/rust:1.65.0@sha256:" + digest + " ",
		"https://docker.io/library/rust:1.65.0@sha256:" + digest,
	} {
		if _, err := NewStandardAuthoringEnvironmentPolicy(image); err == nil {
			t.Fatalf("NewStandardAuthoringEnvironmentPolicy(%q) succeeded, want validation failure", image)
		}
	}

	for _, image := range []string{
		"docker.io/library/rust@sha256:" + digest,
		"ghcr.io/tower-rs/tower-http:v0.5.2@sha256:" + digest,
		"registry.example.test:5000/team/runtime:1.0@sha256:" + digest,
	} {
		if _, err := NewStandardAuthoringEnvironmentPolicy(image); err != nil {
			t.Fatalf("NewStandardAuthoringEnvironmentPolicy(%q) = %v, want success", image, err)
		}
	}
}

func TestParseStandardAuthoringEnvironmentPolicyJSONIsStrict(t *testing.T) {
	valid := `{"format":"harbor.standard-authoring-environment-policy.v1","version":"1","base_image":"` + standardAuthoringPolicyTestImage + `"}`
	invalid := []string{
		`{"format":"harbor.standard-authoring-environment-policy.v1","version":"1","base_image":"` + standardAuthoringPolicyTestImage + `","unexpected":true}`,
		`{"format":"harbor.standard-authoring-environment-policy.v1","version":"1","base_image":"` + standardAuthoringPolicyTestImage + `","base_image":"` + standardAuthoringPolicyTestImage + `"}`,
		valid + ` {}`,
		`null`,
	}
	for _, raw := range invalid {
		if _, err := ParseStandardAuthoringEnvironmentPolicyJSON([]byte(raw)); err == nil {
			t.Fatalf("ParseStandardAuthoringEnvironmentPolicyJSON(%s) succeeded, want failure", raw)
		}
	}
}

func TestValidateDockerfileBaseImage(t *testing.T) {
	policy, err := NewStandardAuthoringEnvironmentPolicy(standardAuthoringPolicyTestImage)
	if err != nil {
		t.Fatal(err)
	}
	other := "docker.io/library/debian:bookworm@sha256:" + strings.Repeat("b", 64)
	for _, test := range []struct {
		name       string
		dockerfile string
		valid      bool
	}{
		{name: "single stage", dockerfile: "FROM " + policy.BaseImage + "\nRUN true\n", valid: true},
		{name: "same image multi stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN true\nFROM " + policy.BaseImage + " AS final\n", valid: true},
		{name: "local multi stage sources", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN true\nFROM " + policy.BaseImage + " AS final\nCOPY --from=build /bin/true /tmp/true\nRUN --mount=type=bind,from=build,target=/src true\n", valid: true},
		{name: "local numeric multi stage source", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN true\nFROM " + policy.BaseImage + "\nCOPY --from=0 /bin/true /tmp/true\n", valid: true},
		{name: "case insensitive local stage alias", dockerfile: "FROM " + policy.BaseImage + " AS Build\nRUN true\nFROM " + policy.BaseImage + " AS final\nCOPY --from=BUILD /bin/true /tmp/true\nRUN --mount=from=bUiLd,target=/src true\n", valid: true},
		{name: "continued local stage source", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN true\nFROM " + policy.BaseImage + " AS final\nCOPY --from=build \\\n/bin/true /tmp/true\n", valid: true},
		{name: "different image", dockerfile: "FROM " + other + "\n", valid: false},
		{name: "mutable tag", dockerfile: "FROM docker.io/library/rust:1.65.0-bullseye\n", valid: false},
		{name: "substitution", dockerfile: "ARG BASE=" + policy.BaseImage + "\nFROM ${BASE}\n", valid: false},
		{name: "platform flag", dockerfile: "FROM --platform=linux/amd64 " + policy.BaseImage + "\n", valid: false},
		{name: "line continuation", dockerfile: "FROM " + policy.BaseImage + " \\\nAS build\n", valid: false},
		{name: "continuation cannot join a builder flag", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --fr\\\nom=docker.io/library/alpine:latest /bin/sh /tmp/sh\n", valid: false},
		{name: "copy external image", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --from=docker.io/library/alpine:latest /bin/sh /tmp/sh\n", valid: false},
		{name: "add external image", dockerfile: "FROM " + policy.BaseImage + " AS build\nADD --from=docker.io/library/alpine:latest /bin/sh /tmp/sh\n", valid: false},
		{name: "copy escaped external image source", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --fr\\om=docker.io/library/alpine:latest /bin/sh /tmp/sh\n", valid: false},
		{name: "copy quoted external image source", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --fr\"om\"=docker.io/library/alpine:latest /bin/sh /tmp/sh\n", valid: false},
		{name: "copy unknown stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --from=other /bin/sh /tmp/sh\n", valid: false},
		{name: "copy current stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --from=build /bin/sh /tmp/sh\n", valid: false},
		{name: "copy current numeric stage", dockerfile: "FROM " + policy.BaseImage + "\nCOPY --from=0 /bin/sh /tmp/sh\n", valid: false},
		{name: "invalid stage alias", dockerfile: "FROM " + policy.BaseImage + " AS 1build\n", valid: false},
		{name: "run mount external image", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN --mount=type=bind,from=docker.io/library/alpine:latest,target=/src true\n", valid: false},
		{name: "run mount escaped external image", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN --mo\\unt=from=docker.io/library/alpine:latest,target=/src true\n", valid: false},
		{name: "run mount unknown stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN --mount=from=other,target=/src true\n", valid: false},
		{name: "run mount current stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN --mount=from=build,target=/src true\n", valid: false},
		{name: "run mount numeric stage", dockerfile: "FROM " + policy.BaseImage + " AS build\nRUN true\nFROM " + policy.BaseImage + " AS final\nRUN --mount=from=0,target=/src true\n", valid: false},
		{name: "continued copy external image", dockerfile: "FROM " + policy.BaseImage + " AS build\nCOPY --from=docker.io/library/alpine:latest \\\n/bin/sh /tmp/sh\n", valid: false},
		{name: "onbuild", dockerfile: "FROM " + policy.BaseImage + "\nONBUILD RUN true\n", valid: false},
		{name: "alternate escape directive", dockerfile: "# escape=`\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "external syntax frontend", dockerfile: "# syntax=docker.io/docker/dockerfile:1.7\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "compact syntax frontend", dockerfile: "#syntax=docker.io/docker/dockerfile:1.7\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "spaced parser directive", dockerfile: "# Check = skip=JSONArgsRecommended\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "alternate syntax frontend comment", dockerfile: "// syntax=docker.io/docker/dockerfile:1.7\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "bom hidden syntax frontend", dockerfile: "\xef\xbb\xbf#syntax=docker.io/docker/dockerfile:1.7\nFROM " + policy.BaseImage + "\n", valid: false},
		{name: "no from", dockerfile: "RUN true\n", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateDockerfileBaseImage([]byte(test.dockerfile), policy)
			if test.valid && err != nil {
				t.Fatalf("ValidateDockerfileBaseImage = %v, want success", err)
			}
			if !test.valid && err == nil {
				t.Fatal("ValidateDockerfileBaseImage succeeded, want failure")
			}
		})
	}
}

func TestStandardAuthoringCatalogConsumesEnvironmentPolicyOnlyWhereRequired(t *testing.T) {
	template := StandardAuthoringWorkflowTemplate()
	if template.Version != "1.2.0" || template.Catalog.Version != "1.2.0" {
		t.Fatalf("authoring template/catalog version = %s/%s, want 1.2.0/1.2.0", template.Version, template.Catalog.Version)
	}
	if err := template.Validate(); err != nil {
		t.Fatalf("validate authoring template: %v", err)
	}
	profile := explicitProfile(template.Catalog)
	resolved, err := template.Compile(profile)
	if err != nil {
		t.Fatalf("compile authoring template: %v", err)
	}

	consumers := map[workflowkit.StageKey]bool{
		workflowkit.StageKey(TaskDesign):        true,
		workflowkit.StageKey(GenerateTaskFiles): true,
		workflowkit.StageKey(DockerfileGen):     true,
		workflowkit.StageKey(ContentReview):     true,
		workflowkit.StageKey(MaterializeTask):   true,
		workflowkit.StageKey(InstructionGen):    false,
		workflowkit.StageKey(TaskTOMLGen):       false,
		workflowkit.StageKey(SolveGen):          false,
		workflowkit.StageKey(SolutionReview):    false,
	}
	for key, wantsInput := range consumers {
		stage, found := resolved.Descriptor.Stage(key)
		if !found {
			t.Fatalf("compiled authoring descriptor is missing stage %q", key)
		}
		input, hasInput := artifactSpecNamed(stage.Inputs, StandardAuthoringEnvironmentPolicyArtifact)
		if hasInput != wantsInput {
			t.Fatalf("stage %q environment policy input presence = %t, want %t", key, hasInput, wantsInput)
		}
		if hasInput && (!input.Required || input.SchemaVersion != StandardAuthoringEnvironmentPolicySchemaVersion) {
			t.Fatalf("stage %q environment policy input = %+v", key, input)
		}
		if resourcePresent(stage.ReadSet, resourceAuthoringEnvironmentPolicy) != wantsInput {
			t.Fatalf("stage %q environment policy read-set membership differs from input contract", key)
		}
	}

	standardDockerfile, found := StandardWorkflowTemplate().Catalog.Stage(workflowkit.StageKey(DockerfileGen))
	if !found {
		t.Fatal("Standard task workflow lacks dockerfile_generate")
	}
	if _, present := artifactSpecNamed(standardDockerfile.Inputs, StandardAuthoringEnvironmentPolicyArtifact); present {
		t.Fatal("task-revision Standard workflow unexpectedly consumes the AuthoringSession environment policy")
	}
}

func artifactSpecNamed(specifications []workflowkit.ArtifactSpec, name string) (workflowkit.ArtifactSpec, bool) {
	for _, specification := range specifications {
		if specification.Name == name {
			return specification, true
		}
	}
	return workflowkit.ArtifactSpec{}, false
}

func resourcePresent(resources []workflowkit.ResourceKey, target workflowkit.ResourceKey) bool {
	for _, resource := range resources {
		if resource == target {
			return true
		}
	}
	return false
}
