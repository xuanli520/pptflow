package taskpolicy

import (
	"strings"
	"testing"
)

func TestValidateStandardAuthoringTaskTOML(t *testing.T) {
	valid := standardAuthoringCurrentHarborTaskTOMLFixture()
	if err := ValidateStandardAuthoringTaskTOML(valid); err != nil {
		t.Fatalf("valid Standard Authoring task.toml: %v", err)
	}

	for _, test := range []struct {
		name    string
		raw     []byte
		wantErr string
	}{
		{name: "metadata only", raw: []byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n"), wantErr: "[task]"},
		{name: "missing Harbor package name", raw: standardAuthoringTaskTOMLWithout(valid, "name = \"tower-rs/tower-http-request-header-count-limit\"\n"), wantErr: "valid org/name"},
		{name: "invalid Harbor package name", raw: standardAuthoringTaskTOMLReplace(valid, "tower-rs/tower-http-request-header-count-limit", "not a package"), wantErr: "valid org/name"},
		{name: "missing actual environment", raw: standardAuthoringTaskTOMLWithout(valid, "[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n"), wantErr: "[environment]"},
		{name: "invalid network mode", raw: standardAuthoringTaskTOMLReplace(valid, "network_mode = \"no-network\"", "network_mode = \"offline\""), wantErr: "network_mode = no-network"},
		{name: "public network mode", raw: standardAuthoringTaskTOMLReplace(valid, "network_mode = \"no-network\"", "network_mode = \"public\""), wantErr: "network_mode = no-network"},
		{name: "different workdir", raw: standardAuthoringTaskTOMLReplace(valid, "workdir = \"/workspace/source\"", "workdir = \"/tmp\""), wantErr: "workdir = /workspace/source"},
		{name: "missing actual verifier", raw: standardAuthoringTaskTOMLWithout(valid, "[verifier]\ntimeout_sec = 1800.0\n"), wantErr: "[verifier]"},
		{name: "ignored verification table", raw: append(append([]byte(nil), valid...), []byte("\n[verification]\ncommands = [\"cargo test --workspace\"]\n")...), wantErr: "must not declare [verification]"},
		{name: "ignored environment dockerfile", raw: standardAuthoringTaskTOMLReplace(valid, "workdir = \"/workspace/source\"", "workdir = \"/workspace/source\"\ndockerfile = \"FROM rust:1.65\""), wantErr: "environment].dockerfile"},
		{name: "old ignored field only shape", raw: []byte("[metadata]\ncode_lang = \"rust\"\ntask_type = \"feature\"\napplication = \"backend\"\nis_0_to_1 = false\n\n[task]\nname = \"tower-rs/tower-http-request-header-count-limit\"\ndescription = \"Add request header limiting middleware.\"\n\n[environment]\ndockerfile = \"FROM rust:1.65\"\n\n[verification]\ncommands = [\"cargo test --workspace\"]\n"), wantErr: "must not declare [verification]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateStandardAuthoringTaskTOML(test.raw)
			if err == nil {
				t.Fatal("invalid Standard Authoring task.toml unexpectedly validated")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validation error = %q, want %q", err, test.wantErr)
			}
		})
	}
}

// This is the Harbor 0.18 core of the observed Tower HTTP task TOML. The
// observed artifact also carried a legacy [verification] block, which is
// intentionally omitted because Harbor ignores it and this policy rejects it.
func standardAuthoringCurrentHarborTaskTOMLFixture() []byte {
	return []byte("schema_version = \"1.0\"\n\n[task]\nname = \"tower-rs/tower-http-request-header-count-limit\"\ndescription = \"Add limit-feature-gated request-header middleware.\"\nkeywords = [\"rust\", \"tower-http\", \"middleware\"]\n\n[metadata]\napplication = \"backend\"\ncode_lang = \"rust\"\nis_0_to_1 = false\ntask_type = \"feature\"\n\n[environment]\nbuild_timeout_sec = 900.0\nnetwork_mode = \"no-network\"\nworkdir = \"/workspace/source\"\n\n[verifier]\ntimeout_sec = 1800.0\n")
}

func standardAuthoringTaskTOMLWithout(content []byte, removed string) []byte {
	return []byte(strings.Replace(string(content), removed, "", 1))
}

func standardAuthoringTaskTOMLReplace(content []byte, old, replacement string) []byte {
	return []byte(strings.Replace(string(content), old, replacement, 1))
}
