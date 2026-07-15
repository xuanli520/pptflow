package codeedge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testRepositoryURL = "https://github.com/acme/widget.git"
	testCommit        = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
)

var testProfile = Profile{Metadata: MetadataFieldMapping{
	CodeLang:    TOMLPath{"metadata", "code_lang"},
	TaskType:    TOMLPath{"metadata", "task_type"},
	Application: TOMLPath{"metadata", "application"},
	IsZeroToOne: TOMLPath{"metadata", "is_0_to_1"},
	GitHubURL:   TOMLPath{"metadata", "github_url"},
	CommitID:    TOMLPath{"metadata", "commit_id"},
}, ProtectedEnvironmentVariables: []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"QWEN_HARBOR_BASE_URL",
	"OPUS_HARBOR_BASE_URL",
}}

func TestInspectPhase1TaskAcceptsManagedDockerTask(t *testing.T) {
	root := writePhase1Task(t, phase1TaskOptions{})

	report, err := inspectPhase1Task(root)
	if err != nil {
		t.Fatalf("InspectPhase1Task() error = %v", err)
	}
	if report.Environment != EnvironmentDockerfile {
		t.Fatalf("environment = %q, want %q", report.Environment, EnvironmentDockerfile)
	}
	if report.Metadata != (Metadata{
		CodeLang:    "go",
		TaskType:    "bug-fix",
		Application: "backend",
		IsZeroToOne: false,
		GitHubURL:   testRepositoryURL,
		CommitID:    testCommit,
	}) {
		t.Fatalf("metadata = %#v, want normalized required metadata", report.Metadata)
	}
	if err := validatePhase1Task(root); err != nil {
		t.Fatalf("ValidatePhase1Task() error = %v", err)
	}
}

func TestInspectPhase1TaskUsesTaskpolicyAndRequiresExactlyOneEnvironment(t *testing.T) {
	t.Run("unexpected managed snapshot file is retained from taskpolicy", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{})
		writeTaskFile(t, root, "README.md", "not part of the managed snapshot\n")

		err := validatePhase1Task(root)
		assertViolationContains(t, err, "task_layout", "unexpected file: README.md")
	})

	t.Run("both Dockerfile and compose are rejected for the CodeEdge profile", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{})
		writeTaskFile(t, root, "environment/docker-compose.yaml", validCompose())

		err := validatePhase1Task(root)
		assertViolationContains(t, err, "environment_profile", "exactly one")
	})
}

func TestInspectPhase1TaskValidatesRequiredMetadataAndNonZeroToOneProvenance(t *testing.T) {
	tests := []struct {
		name          string
		metadata      string
		dockerfile    string
		wantViolation string
	}{
		{
			name: "missing CodeEdge category metadata",
			metadata: `
[metadata]
task_type = "bug-fix"
application = "backend"
is_0_to_1 = true
`,
			dockerfile:    "FROM alpine\n",
			wantViolation: "metadata.code_lang is required",
		},
		{
			name: "non zero to one requires GitHub URL",
			metadata: `
[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
commit_id = "` + testCommit + `"
`,
			wantViolation: "metadata.github_url is required for non-0-1 tasks",
		},
		{
			name: "non zero to one rejects branch in place of commit",
			metadata: `
[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "` + testRepositoryURL + `"
commit_id = "main"
`,
			wantViolation: "metadata.commit_id must be a 7-40 character hexadecimal commit",
		},
		{
			name: "metadata GitHub URL must be a public HTTPS link",
			metadata: `
[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "git@github.com:acme/widget.git"
commit_id = "` + testCommit + `"
`,
			wantViolation: "metadata.github_url must be an HTTPS public GitHub repository URL",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writePhase1Task(t, phase1TaskOptions{taskTOML: test.metadata, dockerfile: test.dockerfile})
			assertViolationContains(t, validatePhase1Task(root), "task_metadata", test.wantViolation)
		})
	}
}

func TestInspectPhase1TaskAllowsZeroToOneWithoutPublicRepositoryMetadata(t *testing.T) {
	root := writePhase1Task(t, phase1TaskOptions{
		taskTOML: `
[metadata]
code_lang = "python"
task_type = "feature"
application = "cli"
is_0_to_1 = true
`,
		dockerfile: "FROM python:3.13-alpine\n",
	})

	report, err := inspectPhase1Task(root)
	if err != nil {
		t.Fatalf("zero-to-one task rejected: %v", err)
	}
	if !report.Metadata.IsZeroToOne || report.Metadata.GitHubURL != "" || report.Metadata.CommitID != "" {
		t.Fatalf("zero-to-one metadata = %#v, want no required provenance", report.Metadata)
	}
}

func TestInspectPhase1TaskUsesExplicitMetadataFieldMapping(t *testing.T) {
	profile := Profile{Metadata: MetadataFieldMapping{
		CodeLang:    TOMLPath{"submission", "language"},
		TaskType:    TOMLPath{"submission", "kind"},
		Application: TOMLPath{"submission", "domain"},
		IsZeroToOne: TOMLPath{"submission", "from_scratch"},
		GitHubURL:   TOMLPath{"submission", "source_repository"},
		CommitID:    TOMLPath{"submission", "source_revision"},
	}, ProtectedEnvironmentVariables: testProfile.ProtectedEnvironmentVariables}
	root := writePhase1Task(t, phase1TaskOptions{taskTOML: `
[submission]
language = "go"
kind = "bug-fix"
domain = "backend"
from_scratch = false
source_repository = "https://github.com/acme/widget.git"
source_revision = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
`})

	report, err := InspectPhase1Task(root, profile)
	if err != nil {
		t.Fatalf("explicit alternate mapping rejected: %v", err)
	}
	if report.Metadata.CodeLang != "go" || report.Metadata.GitHubURL != testRepositoryURL || report.Metadata.CommitID != testCommit {
		t.Fatalf("report metadata = %#v, want values from the explicit mapping", report.Metadata)
	}
}

func TestInspectPhase1TaskRejectsAnImplicitOrAmbiguousMetadataProfile(t *testing.T) {
	root := writePhase1Task(t, phase1TaskOptions{})
	err := ValidatePhase1Task(root, Profile{})
	assertViolationContains(t, err, "metadata_profile", "metadata field mapping is required for code language")
}

func TestInspectPhase1TaskValidatesTestsAnalysisStructure(t *testing.T) {
	t.Run("missing required section", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{testsAnalysis: `
## 1. instruction 和 environment 已提供的信息
- visible contract

## 3. 模型具备通过条件的依据
- verifier is derivable
`})
		assertViolationContains(t, validatePhase1Task(root), "tests_analysis", "missing required section: 模型的理论通过路径")
	})

	t.Run("sections must be ordered and substantive", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{testsAnalysis: `
## 2. 模型的理论通过路径
- work through the task

## 1. instruction 和 environment 已提供的信息

## 3. 模型具备通过条件的依据
- visible success condition
`})
		err := validatePhase1Task(root)
		assertViolationContains(t, err, "tests_analysis", "documented 1, 2, 3 order")
		assertViolationContains(t, err, "tests_analysis", "must contain substantive content: instruction 和 environment 已提供的信息")
	})

	t.Run("extra analysis after the required template is allowed", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{testsAnalysis: validTestsAnalysis() + `

## 附录：审核备注
- This does not replace any required section.
`})
		if err := validatePhase1Task(root); err != nil {
			t.Fatalf("analysis with an appendix rejected: %v", err)
		}
	})
}

func TestInspectPhase1TaskRejectsObviousDockerfileIsolationLeaks(t *testing.T) {
	tests := []struct {
		name       string
		dockerfile string
		want       string
	}{
		{
			name: "tests copied into image",
			dockerfile: `
FROM alpine
COPY tests /opt/tests
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git checkout ` + testCommit + `
`,
			want: "COPY must not include tests",
		},
		{
			name: "solution added into image JSON form",
			dockerfile: `
FROM alpine
ADD ["solution/solve.sh", "/opt/solve.sh"]
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git checkout ` + testCommit + `
`,
			want: "ADD must not include solution",
		},
		{
			name: "root wildcard source is not auditable",
			dockerfile: `
FROM alpine
COPY . /app
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git checkout ` + testCommit + `
`,
			want: "COPY must not use a broad task-root or wildcard source: .",
		},
		{
			name: "verifier reward is written during build",
			dockerfile: `
FROM alpine
RUN touch /logs/verifier/reward.txt
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git checkout ` + testCommit + `
`,
			want: "must not prewrite or include verifier reward files",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writePhase1Task(t, phase1TaskOptions{dockerfile: test.dockerfile})
			assertViolationContains(t, validatePhase1Task(root), "environment_isolation", test.want)
		})
	}
}

func TestInspectPhase1TaskRejectsProtectedDeploymentEnvironmentReferences(t *testing.T) {
	t.Run("Dockerfile rejects protected interpolation and declaration", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{dockerfile: `
FROM alpine
ARG QWEN_HARBOR_BASE_URL
RUN printf '%s' "${ANTHROPIC_AUTH_TOKEN}"
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git checkout ` + testCommit + `
`})
		err := validatePhase1Task(root)
		assertViolationContains(t, err, "environment_isolation", "Dockerfile must not declare protected deployment environment variable QWEN_HARBOR_BASE_URL")
		assertViolationContains(t, err, "environment_isolation", "Dockerfile must not interpolate protected deployment environment variable ANTHROPIC_AUTH_TOKEN")
	})

	t.Run("Dockerfile rejects a non-default escape directive", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{dockerfile: "# escape=`\nARG ANTHROPIC_`\nAUTH_TOKEN\nFROM alpine\nRUN git clone " + testRepositoryURL + " /app/repo && cd /app/repo && git checkout " + testCommit + "\n"})
		assertViolationContains(t, validatePhase1Task(root), "environment_isolation", "Dockerfile must not use a non-default escape directive")
	})

	t.Run("Compose rejects protected pass-through and interpolation", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{useCompose: true, compose: `
services:
  main:
    build:
      context: .
      args:
        OPUS_HARBOR_BASE_URL:
    environment:
      ANTHROPIC_AUTH_TOKEN:
      APP_ENDPOINT: "${QWEN_HARBOR_BASE_URL:-https://example.invalid}"
    env_file:
      - ANTHROPIC_BASE_URL
`})
		err := validatePhase1Task(root)
		assertViolationContains(t, err, "environment_isolation", "Compose must not declare protected deployment environment variable OPUS_HARBOR_BASE_URL")
		assertViolationContains(t, err, "environment_isolation", "Compose must not declare protected deployment environment variable ANTHROPIC_AUTH_TOKEN")
		assertViolationContains(t, err, "environment_isolation", "Compose must not interpolate protected deployment environment variable QWEN_HARBOR_BASE_URL")
		assertViolationContains(t, err, "environment_isolation", "Compose must not declare protected deployment environment variable ANTHROPIC_BASE_URL")
	})

	t.Run("unrelated application interpolation remains allowed", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{useCompose: true, compose: `
services:
  main:
    build:
      context: .
      dockerfile_inline: |
        FROM alpine
        RUN git clone https://github.com/acme/widget.git /app/repo && cd /app/repo && git checkout a1b2c3d4e5f60718293a4b5c6d7e8f9012345678
    environment:
      APP_MODE: "${APP_MODE:-production}"
`})
		if err := validatePhase1Task(root); err != nil {
			t.Fatalf("unrelated compose interpolation rejected: %v", err)
		}
	})
}

func TestProtectedEnvironmentReferencesRejectHarborTaskEnvironmentTemplates(t *testing.T) {
	tests := []struct {
		name     string
		taskTOML string
		want     string
	}{
		{
			name: "direct protected definition",
			taskTOML: validTaskTOML() + `
[environment.env]
ANTHROPIC_AUTH_TOKEN = "literal-task-value"
`,
			want: "task.toml [environment.env] must not declare protected deployment environment variable ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "protected bare pass-through",
			taskTOML: validTaskTOML() + `
[environment.env]
ANTHROPIC_AUTH_TOKEN = "${ANTHROPIC_AUTH_TOKEN}"
`,
			want: "task.toml [environment.env] must not declare protected deployment environment variable ANTHROPIC_AUTH_TOKEN",
		},
		{
			name: "protected alias interpolation",
			taskTOML: validTaskTOML() + `
[environment.env]
LEAK = "${ANTHROPIC_AUTH_TOKEN:-fallback}"
`,
			want: "task.toml [environment.env] must not interpolate protected deployment environment variable ANTHROPIC_AUTH_TOKEN",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writePhase1Task(t, phase1TaskOptions{taskTOML: test.taskTOML})
			err := ValidateProtectedEnvironmentReferences(root, testProfile.ProtectedEnvironmentVariables)
			assertViolationContains(t, err, "environment_isolation", test.want)
			assertViolationContains(t, validatePhase1Task(root), "environment_isolation", test.want)
		})
	}

	t.Run("unrelated task environment interpolation remains allowed", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{taskTOML: validTaskTOML() + `
[environment.env]
APP_MODE = "${APP_MODE:-production}"
`})
		if err := ValidateProtectedEnvironmentReferences(root, testProfile.ProtectedEnvironmentVariables); err != nil {
			t.Fatalf("unrelated task environment interpolation rejected: %v", err)
		}
	})
}

func TestInspectPhase1TaskChecksGitCloneAndCommitAgainstMetadata(t *testing.T) {
	tests := []struct {
		name       string
		dockerfile string
		want       string
	}{
		{
			name:       "missing clone",
			dockerfile: "FROM alpine\n",
			want:       "must git clone the mapped GitHub URL",
		},
		{
			name: "wrong source repository",
			dockerfile: `
FROM alpine
RUN git clone https://github.com/acme/other.git /app/repo && cd /app/repo && git checkout ` + testCommit + `
`,
			want: "git clone repository must match the mapped GitHub URL",
		},
		{
			name: "branch without explicit commit",
			dockerfile: `
FROM alpine
RUN git clone --branch main ` + testRepositoryURL + ` /app/repo
`,
			want: "must checkout or reset the mapped commit ID",
		},
		{
			name: "checkout mismatch",
			dockerfile: `
FROM alpine
RUN git clone ` + testRepositoryURL + ` /app/repo && cd /app/repo && git reset --hard deadbeef
`,
			want: "git checkout/reset commit must match the mapped commit ID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writePhase1Task(t, phase1TaskOptions{dockerfile: test.dockerfile})
			assertViolationContains(t, validatePhase1Task(root), "repo_provenance", test.want)
		})
	}

	t.Run("normalizes a public SSH clone and accepts reset", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{dockerfile: `
FROM alpine
RUN git clone git@github.com:acme/widget.git /app/repo && cd /app/repo && git reset --hard ` + testCommit + `
`})
		if err := validatePhase1Task(root); err != nil {
			t.Fatalf("fixed SSH clone rejected: %v", err)
		}
	})

	t.Run("normalizes an ssh URL clone and accepts checkout", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{dockerfile: `
FROM alpine
RUN git clone ssh://git@github.com/acme/widget.git /app/repo && cd /app/repo && git checkout ` + testCommit + `
`})
		if err := validatePhase1Task(root); err != nil {
			t.Fatalf("fixed ssh URL clone rejected: %v", err)
		}
	})
}

func TestInspectPhase1TaskValidatesComposeIsolationAndInlineProvenance(t *testing.T) {
	t.Run("safe compose task with inline Dockerfile provenance", func(t *testing.T) {
		root := writePhase1Task(t, phase1TaskOptions{
			useCompose: true,
			compose: `
services:
  main:
    build:
      context: .
      dockerfile_inline: |
        FROM alpine
        RUN git clone https://github.com/acme/widget.git /app/repo && cd /app/repo && git checkout a1b2c3d4e5f60718293a4b5c6d7e8f9012345678
`,
		})
		report, err := inspectPhase1Task(root)
		if err != nil {
			t.Fatalf("safe compose task rejected: %v", err)
		}
		if report.Environment != EnvironmentCompose {
			t.Fatalf("environment = %q, want %q", report.Environment, EnvironmentCompose)
		}
	})

	tests := []struct {
		name    string
		compose string
		want    string
	}{
		{
			name: "main service is mandatory",
			compose: `
services:
  worker:
    build:
      context: .
`,
			want: "must define a main service",
		},
		{
			name: "task root context is forbidden",
			compose: `
services:
  main:
    build:
      context: ..
`,
			want: "must not escape the managed environment directory",
		},
		{
			name: "tests cannot be mounted",
			compose: `
services:
  main:
    build:
      context: .
    volumes:
      - ./tests:/opt/tests
`,
			want: "compose volumes must not mount tests",
		},
		{
			name: "inline Dockerfile cannot copy solution",
			compose: `
services:
  main:
    build:
      context: .
      dockerfile_inline: |
        FROM alpine
        COPY solution /opt/solution
`,
			want: "COPY must not include solution",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writePhase1Task(t, phase1TaskOptions{useCompose: true, compose: test.compose})
			assertViolationContains(t, validatePhase1Task(root), "environment_isolation", test.want)
		})
	}
}

func TestInspectPhase1TaskProducesDeterministicSortedDiagnostics(t *testing.T) {
	root := writePhase1Task(t, phase1TaskOptions{
		taskTOML: `
[metadata]
code_lang = ""
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "https://github.com/acme/widget.git"
commit_id = "main"
`,
		dockerfile: "FROM alpine\nCOPY tests /opt/tests\n",
	})

	first := validationError(t, validatePhase1Task(root))
	second := validationError(t, validatePhase1Task(root))
	if first.Error() != second.Error() {
		t.Fatalf("diagnostics are not deterministic:\nfirst:  %s\nsecond: %s", first, second)
	}
	for index := 1; index < len(first.Violations); index++ {
		previous := first.Violations[index-1]
		current := first.Violations[index]
		if previous.Code > current.Code || (previous.Code == current.Code && previous.Path > current.Path) || (previous.Code == current.Code && previous.Path == current.Path && previous.Message > current.Message) {
			t.Fatalf("violations are not sorted: %#v before %#v", previous, current)
		}
	}
}

type phase1TaskOptions struct {
	taskTOML      string
	testsAnalysis string
	dockerfile    string
	compose       string
	useCompose    bool
}

func writePhase1Task(t *testing.T, options phase1TaskOptions) string {
	t.Helper()
	if options.taskTOML == "" {
		options.taskTOML = validTaskTOML()
	}
	if options.testsAnalysis == "" {
		options.testsAnalysis = validTestsAnalysis()
	}
	if options.dockerfile == "" {
		options.dockerfile = validDockerfile()
	}
	if options.compose == "" {
		options.compose = validCompose()
	}
	root := t.TempDir()
	writeTaskFile(t, root, "instruction.md", "Implement the documented behavior without reading verifier internals.\n")
	writeTaskFile(t, root, "task.toml", strings.TrimSpace(options.taskTOML)+"\n")
	writeTaskFile(t, root, "tests_analysis.md", strings.TrimSpace(options.testsAnalysis)+"\n")
	writeTaskFile(t, root, "solution/solve.sh", "#!/bin/sh\nexit 0\n")
	writeTaskFile(t, root, "tests/test.sh", "#!/bin/sh\nexit 0\n")
	if options.useCompose {
		writeTaskFile(t, root, "environment/docker-compose.yaml", strings.TrimSpace(options.compose)+"\n")
	} else {
		writeTaskFile(t, root, "environment/Dockerfile", strings.TrimSpace(options.dockerfile)+"\n")
	}
	return root
}

func writeTaskFile(t *testing.T, root, relativePath, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validTaskTOML() string {
	return `
schema_version = "1.3"

[task]
name = "codeedge/example"

[metadata]
code_lang = "go"
task_type = "bug-fix"
application = "backend"
is_0_to_1 = false
github_url = "` + testRepositoryURL + `"
commit_id = "` + testCommit + `"
`
}

func validTestsAnalysis() string {
	return `
## 1. instruction 和 environment 已提供的信息
- The instruction defines the visible behavior and the environment gives the fixed repository revision.

---

## 2. 模型的理论通过路径
- Read the visible source, implement the documented behavior, and run the documented verification.

---

## 3. 模型具备通过条件的依据
- The verifier checks only behavior derivable from the instruction and environment.
`
}

func validDockerfile() string {
	return `
FROM alpine:3.22
RUN git clone "` + testRepositoryURL + `" /app/repo && cd /app/repo && git checkout "` + testCommit + `"
WORKDIR /app/repo
`
}

func validCompose() string {
	return `
services:
  main:
    build:
      context: .
`
}

func validatePhase1Task(root string) error {
	return ValidatePhase1Task(root, testProfile)
}

func inspectPhase1Task(root string) (Report, error) {
	return InspectPhase1Task(root, testProfile)
}

func assertViolationContains(t *testing.T, err error, code, want string) {
	t.Helper()
	validation := validationError(t, err)
	for _, violation := range validation.Violations {
		if violation.Code == code && strings.Contains(violation.Message, want) {
			return
		}
	}
	t.Fatalf("violations = %#v, want code %q containing %q", validation.Violations, code, want)
}

func validationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	if err == nil {
		t.Fatal("preflight unexpectedly succeeded")
	}
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error type = %T (%v), want *ValidationError", err, err)
	}
	return validation
}
