package docker_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

func TestRedactLogTextRemovesSecrets(t *testing.T) {
	input := strings.Join([]string{
		"POSTGRES_PASSWORD=super-secret",
		"api_key: abc123",
		"Authorization: Bearer token-value",
		"image: http://user:pass@example.com/app",
		"jwt=eyJabc.def.ghi",
	}, "\n")
	output := dockermgr.RedactLogText(input)
	for _, secret := range []string{"super-secret", "abc123", "token-value", "user:pass", "eyJabc.def.ghi"} {
		if strings.Contains(output, secret) {
			t.Fatalf("redacted output still contains %q: %s", secret, output)
		}
	}
	if count := strings.Count(output, "[REDACTED]"); count < 4 {
		t.Fatalf("expected redaction markers, got %d in %s", count, output)
	}
}

func TestStartRuntimeRedactsDockerLogAndProgress(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{configOutput: "services:\n  db:\n    environment:\n      POSTGRES_PASSWORD: super-secret\n"}
	cfg := config.Default().Docker
	cfg.BuildMirrors.Enabled = false
	var log strings.Builder
	var progress []string
	_, _ = (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Log:          &log,
		Progress: func(event dockermgr.ProgressEvent) {
			progress = append(progress, event.Line)
		},
		Timeouts: dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	combined := log.String() + "\n" + strings.Join(progress, "\n")
	if strings.Contains(combined, "super-secret") {
		t.Fatalf("docker evidence leaked secret:\n%s", combined)
	}
	if !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("docker evidence missing redaction marker:\n%s", combined)
	}
}

func TestStartRuntimeRedactsDockerLogAcrossSplitWrites(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{
		configOutput: "services:\n  db:\n    environment:\n      POSTGRES_PASSWORD: super-secret\n",
		streamChunks: []string{"POSTGRES_PASS", "WORD=super-secret"},
	}
	cfg := config.Default().Docker
	cfg.BuildMirrors.Enabled = false
	var log strings.Builder
	_, _ = (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Log:          &log,
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if strings.Contains(log.String(), "super-secret") {
		t.Fatalf("docker log leaked split secret:\n%s", log.String())
	}
	if !strings.Contains(log.String(), "POSTGRES_PASSWORD=[REDACTED]") {
		t.Fatalf("docker log missing redacted split secret:\n%s", log.String())
	}
}

func TestStartRuntimePullPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     string
		wantErr    bool
		wantPull   bool
		wantStatus string
	}{
		{name: "best effort continues", policy: "best_effort", wantPull: true, wantStatus: "warning"},
		{name: "required fails", policy: "required", wantErr: true, wantPull: true, wantStatus: "failed"},
		{name: "skip omits pull", policy: "skip", wantPull: false, wantStatus: "skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := writeComposeProject(t, t.TempDir())
			runner := &scriptedDockerRunner{pullErr: true}
			cfg := config.Default().Docker
			cfg.PullPolicy = tc.policy
			cfg.BuildMirrors.Enabled = false

			result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
				RepoPath:     repo,
				ArtifactRoot: t.TempDir(),
				TaskID:       "TASK-1",
				RunID:        "run-1",
				Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
			})
			if tc.wantErr && err == nil {
				t.Fatal("expected pull policy error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := containsCommand(runner.commands, " pull "); got != tc.wantPull {
				t.Fatalf("pull command presence = %t, want %t; commands=%#v", got, tc.wantPull, runner.commands)
			}
			if result.RuntimeSummary.Pull.Status != tc.wantStatus {
				t.Fatalf("pull status = %q, want %q", result.RuntimeSummary.Pull.Status, tc.wantStatus)
			}
		})
	}
}

func TestStartRuntimeBuildMirrorPatchWritesArtifactsWithoutModifyingRepo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	web := filepath.Join(repo, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  web:
    build:
      context: ./web
      dockerfile: Dockerfile
      target: production
      args:
        NODE_ENV: production
`
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	original := "# syntax=docker/dockerfile:1\nFROM node:20 AS frontend\nRUN npm ci\nFROM python:3.12\nRUN python -m pip install -r requirements.txt\n"
	dockerfile := filepath.Join(web, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: artifactRoot,
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if len(result.Runtime.ComposeFiles) != 2 || !strings.HasSuffix(result.Runtime.ComposeFiles[1], "compose.mirror.override.yml") {
		t.Fatalf("runtime should use original compose plus mirror override: files=%#v mirror=%#v", result.Runtime.ComposeFiles, result.MirrorSummary)
	}
	if !result.MirrorSummary.OverrideGenerated || !result.MirrorSummary.OverrideVerified {
		t.Fatalf("mirror override should be generated and verified: %#v", result.MirrorSummary)
	}
	patchedPath := filepath.Join(artifactRoot, "docker_mirror", "web.Dockerfile.p2r")
	patched, err := os.ReadFile(patchedPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ENV NPM_CONFIG_REGISTRY=", "ENV PIP_INDEX_URL="} {
		if !strings.Contains(string(patched), want) {
			t.Fatalf("patched Dockerfile missing %q:\n%s", want, patched)
		}
	}
	current, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != original {
		t.Fatalf("repo Dockerfile was modified:\n%s", current)
	}
}

func TestStartRuntimeBuildMirrorFallsBackWhenPatchedDockerfileOutsideContext(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	web := filepath.Join(repo, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  web:\n    build: ./web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "Dockerfile"), []byte("FROM node:20\nRUN npm ci\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{rejectMirrorBuildOutsideContext: true}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:          repo,
		ArtifactRoot:      filepath.Join(root, "artifacts"),
		TaskID:            "TASK-1",
		RunID:             "run-1",
		RewriteFixedPorts: true,
		Timeouts:          dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("fallback build should keep runtime startup alive: %v", err)
	}
	if !result.MirrorSummary.FallbackUsed || result.MirrorSummary.FallbackReason != "dockerfile_path_outside_context" {
		t.Fatalf("mirror fallback should record outside-context reason: %#v", result.MirrorSummary)
	}
	if len(result.Runtime.ComposeFiles) != 1 || strings.Contains(strings.Join(result.Runtime.ComposeFiles, " "), "compose.mirror.override.yml") {
		t.Fatalf("runtime should fall back to base compose files: %#v", result.Runtime.ComposeFiles)
	}
}

func TestStartRuntimeBuildMirrorNormalizesDebianMultiarchCopyPath(t *testing.T) {
	targetDir := testDebianMultiarchLibDir()
	if targetDir == "" {
		t.Skip("unsupported test architecture")
	}
	sourceDir := "aarch64-linux-gnu"
	if sourceDir == targetDir {
		sourceDir = "x86_64-linux-gnu"
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	backend := filepath.Join(repo, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  backend:\n    build: ./backend\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM postgres:16 AS pg\nFROM python:3.11-slim\nCOPY --from=pg /usr/lib/" + sourceDir + "/libpq.so* /usr/lib/" + sourceDir + "/\nRUN apt-get update && apt-get install -y curl\n"
	if err := os.WriteFile(filepath.Join(backend, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := filepath.Join(root, "artifacts")
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: artifactRoot,
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	patchedPath := filepath.Join(artifactRoot, "docker_mirror", "backend.Dockerfile.p2r")
	patched, err := os.ReadFile(patchedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "/usr/lib/"+targetDir+"/libpq.so*") || strings.Contains(string(patched), "/usr/lib/"+sourceDir+"/libpq.so*") {
		t.Fatalf("patched Dockerfile did not normalize multiarch path:\n%s", patched)
	}
	if len(result.MirrorSummary.Services) != 1 || !containsString(result.MirrorSummary.Services[0].Warnings, "normalized Debian multiarch COPY path") {
		t.Fatalf("multiarch normalization should be recorded: %#v", result.MirrorSummary.Services)
	}
}

func TestFindComposeRecursesDeterministicallyAfterTopLevelPriority(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "docker", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker", "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker", "nested", "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dockermgr.FindCompose(repo); got != filepath.Join(repo, "docker", "compose.yml") {
		t.Fatalf("recursive compose = %q, want shallower deterministic candidate", got)
	}
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := dockermgr.FindCompose(repo); got != filepath.Join(repo, "compose.yaml") {
		t.Fatalf("top-level compose = %q, want top-level priority", got)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "node_modules", "pkg", "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if got := dockermgr.FindCompose(repo); strings.Contains(got, "node_modules") {
		t.Fatalf("recursive compose should skip dependency directories, got %q", got)
	}
}

func TestFindComposeFilesIncludesDefaultOverride(t *testing.T) {
	repo := t.TempDir()
	base := filepath.Join(repo, "docker-compose.yml")
	override := filepath.Join(repo, "docker-compose.override.yml")
	if err := os.WriteFile(base, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("services:\n  web:\n    environment:\n      APP_ENV: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := dockermgr.FindComposeFiles(repo)
	if strings.Join(files, ",") != base+","+override {
		t.Fatalf("compose files = %#v, want base plus default override", files)
	}
}

func TestStartRuntimeUsesDeclaredComposeProjectName(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("name: piw8e0fa7\nservices:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.Runtime.ComposeProject != "piw8e0fa7" || result.RuntimeSummary.ComposeProjectSource != "compose_name" {
		t.Fatalf("compose project selection should honor declared name: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if containsCommand(runner.commands, "-p p2rqa_") || !containsCommand(runner.commands, "-p piw8e0fa7") {
		t.Fatalf("docker commands should use declared compose project name: %#v", runner.commands)
	}
	if !containsCommand(runner.commands, " up -d --build") {
		t.Fatalf("runtime startup should use up --build semantics: %#v", runner.commands)
	}
}

func TestStartRuntimeLayersDefaultComposeOverride(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(repo, "docker-compose.yml")
	override := filepath.Join(repo, "docker-compose.override.yml")
	if err := os.WriteFile(base, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("services:\n  web:\n    environment:\n      APP_ENV: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if !stringSliceContains(result.Runtime.ComposeFiles, base) || !stringSliceContains(result.Runtime.ComposeFiles, override) {
		t.Fatalf("runtime compose files should include default override: %#v", result.Runtime.ComposeFiles)
	}
	if !containsCommand(runner.commands, "-f "+base+" -f "+override+" ") {
		t.Fatalf("docker commands should layer default override like manual compose: %#v", runner.commands)
	}
}

func TestStartRuntimeUsesComposeProjectNameFromEnvFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("COMPOSE_PROJECT_NAME=envproj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.Runtime.ComposeProject != "envproj" || result.RuntimeSummary.ComposeProjectSource != "compose_project_env" {
		t.Fatalf("COMPOSE_PROJECT_NAME should match manual compose precedence: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if containsCommand(runner.commands, "-p p2rqa_") || !containsCommand(runner.commands, "-p envproj") {
		t.Fatalf("docker commands should use COMPOSE_PROJECT_NAME project: %#v", runner.commands)
	}
}

func TestStartRuntimeUsesComposeProjectNameFromDefaultOverride(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(repo, "docker-compose.yml")
	override := filepath.Join(repo, "docker-compose.override.yml")
	if err := os.WriteFile(base, []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, []byte("name: overrideproj\nservices:\n  web:\n    environment:\n      APP_ENV: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.Runtime.ComposeProject != "overrideproj" || result.RuntimeSummary.ComposeProjectSource != "compose_name" {
		t.Fatalf("compose override name should be honored: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if !containsCommand(runner.commands, "-f "+base+" -f "+override+" -p overrideproj") {
		t.Fatalf("docker commands should layer override and use its project name: %#v", runner.commands)
	}
}

func TestStartRuntimeKeepsGeneratedProjectWhenComposeNameIsImplicit(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.RuntimeSummary.ComposeProjectSource != "generated" || !strings.HasPrefix(result.Runtime.ComposeProject, "p2rqa_") {
		t.Fatalf("implicit compose project should keep p2r isolation: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
}

func TestStartRuntimeResolvesComposeProjectNameFromGeneratedEnvFile(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := "name: ${COMPOSE_PROJECT_NAME:-fallbackproj}\nservices:\n  api:\n    image: nginx\n    environment:\n      APP_ENV: ${APP_ENV}\n"
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("COMPOSE_PROJECT_NAME=exampleproj\nAPP_ENV=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.Runtime.ComposeProject != "exampleproj" || result.RuntimeSummary.ComposeProjectSource != "compose_project_env" {
		t.Fatalf("generated env file should drive compose project selection: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if len(result.Runtime.EnvFiles) != 1 || !containsCommand(runner.commands, "--env-file "+result.Runtime.EnvFiles[0]+" ") {
		t.Fatalf("docker commands should use generated env file: runtime=%#v commands=%#v", result.Runtime, runner.commands)
	}
	if containsCommand(runner.commands, "-p fallbackproj") || !containsCommand(runner.commands, "-p exampleproj") {
		t.Fatalf("docker commands should use env-resolved project: %#v", runner.commands)
	}
}

func TestStartRuntimeSkipsHardPrebuildWithoutMirrorOverride(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.RuntimeSummary.Build.Status != "skipped" {
		t.Fatalf("prebuild should be skipped when no p2r mirror override is active: %#v", result.RuntimeSummary.Build)
	}
	if containsExplicitComposeBuildCommand(runner.commands) || !containsCommand(runner.commands, " up -d --build") {
		t.Fatalf("startup should rely on up --build without separate hard build: %#v", runner.commands)
	}
}

func TestStartRuntimeCopiesEnvExampleBeforeComposeConfig(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  api:
    image: nginx
    env_file:
      - path: .env
        required: false
        format: raw
      - path: ./optional.env
        required: false
    environment:
      APP_ENV: ${APP_ENV}
`
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("APP_ENV=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:          repo,
		ArtifactRoot:      filepath.Join(root, "artifacts"),
		TaskID:            "TASK-1",
		RunID:             "run-1",
		RewriteFixedPorts: true,
		Timeouts:          dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".env")); !os.IsNotExist(err) {
		t.Fatalf("runtime env preparation must not write repo .env, stat err: %v", err)
	}
	envPath := filepath.Join(root, "artifacts", "docker_runtime", "runtime.env")
	content, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "APP_ENV=test\n" {
		t.Fatalf("runtime env content = %q", content)
	}
	if len(result.RuntimeSummary.EnvFilesPrepared) != 1 || result.RuntimeSummary.EnvFilesPrepared[0] != envPath {
		t.Fatalf("env preparation not recorded: %#v", result.RuntimeSummary.EnvFilesPrepared)
	}
	if !stringSliceContains(result.Runtime.ComposeFiles, filepath.Join(root, "artifacts", "docker_runtime", "compose.env.yml")) || !stringSliceContains(result.Runtime.EnvFiles, envPath) {
		t.Fatalf("runtime should use artifact env file and compose override: runtime=%#v", result.Runtime)
	}
	overrideContent, err := os.ReadFile(filepath.Join(root, "artifacts", "docker_runtime", "compose.env.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{envPath, "required: false", "format: raw", "optional.env"} {
		if !strings.Contains(string(overrideContent), want) {
			t.Fatalf("env override should preserve long syntax %q:\n%s", want, overrideContent)
		}
	}
	if !containsCommand(runner.commands, "--env-file "+envPath+" ") || !containsCommand(runner.commands, "compose.env.yml") {
		t.Fatalf("compose commands should use artifact env preparation: %#v", runner.commands)
	}
}

func TestStartRuntimeRewritesFixedHostPortsToComposeAllocatedPorts(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  db:
    image: postgres:16
    ports:
      - "5432:5432"
  api:
    image: api
    ports:
      - target: 8000
        published: "8000"
        protocol: tcp
`
	composePath := filepath.Join(repo, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:          repo,
		ArtifactRoot:      filepath.Join(root, "artifacts"),
		TaskID:            "TASK-1",
		RunID:             "run-1",
		RewriteFixedPorts: true,
		Timeouts:          dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.RuntimeSummary.PortRewrite == nil || !result.RuntimeSummary.PortRewrite.Generated {
		t.Fatalf("fixed ports should generate a runtime port rewrite: %#v", result.RuntimeSummary.PortRewrite)
	}
	if !containsCommand(runner.commands, " --project-directory "+repo+" ") {
		t.Fatalf("docker commands should use original project directory: %#v", runner.commands)
	}
	if !containsCommand(runner.commands, composePath+" -f "+result.RuntimeSummary.PortRewrite.ComposeFile+" ") {
		t.Fatalf("docker commands should layer original compose plus ports override: %#v", runner.commands)
	}
	content, err := os.ReadFile(result.RuntimeSummary.PortRewrite.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "5432:5432") || strings.Contains(string(content), "published:") {
		t.Fatalf("rewritten compose should not retain fixed published ports:\n%s", content)
	}
	for _, want := range []string{"ports: !override", "- \"5432\"", "- 8000/tcp"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("rewritten compose missing %q:\n%s", want, content)
		}
	}
	original, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), "5432:5432") {
		t.Fatalf("source compose should not be modified:\n%s", original)
	}
}

func TestStartRuntimeAddsDeterministicNetworkIPAMOverride(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	composePath := filepath.Join(repo, "docker-compose.yml")
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: nginx\n    networks:\n      - appnet\nnetworks:\n  appnet: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{configOutput: "services:\n  web:\n    image: nginx\n    networks:\n      appnet: null\nnetworks:\n  appnet:\n    name: p2r_test_appnet\n"}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.RuntimeSummary.NetworkIPAM == nil || !result.RuntimeSummary.NetworkIPAM.Generated {
		t.Fatalf("network ipam override should be generated: %#v", result.RuntimeSummary.NetworkIPAM)
	}
	if len(result.RuntimeSummary.NetworkIPAM.Networks) != 1 || result.RuntimeSummary.NetworkIPAM.Networks[0].Network != "appnet" {
		t.Fatalf("network ipam summary should target appnet: %#v", result.RuntimeSummary.NetworkIPAM)
	}
	content, err := os.ReadFile(result.RuntimeSummary.NetworkIPAM.ComposeFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"appnet:", "ipam:", "subnet: 100."} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("network ipam override missing %q:\n%s", want, content)
		}
	}
	if !containsCommand(runner.commands, composePath+" -f "+result.RuntimeSummary.NetworkIPAM.ComposeFile+" ") {
		t.Fatalf("docker commands should layer original compose plus network override: %#v", runner.commands)
	}
}

func TestStartRuntimeAddsManagedLabelOverride(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	artifactRoot := t.TempDir()
	runner := &scriptedDockerRunner{configOutput: "services:\n  api:\n    image: nginx\n  web:\n    image: nginx\n"}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: artifactRoot,
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Labels: map[string]string{
			"managed_by":  "p2rqa",
			"p2r.task_id": "TASK-1",
		},
		Timeouts: dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	overridePath := filepath.Join(artifactRoot, "runtime_labels.compose.yml")
	content, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api:", "web:", "managed_by: p2rqa", "p2r.task_id: TASK-1"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("label override missing %q:\n%s", want, content)
		}
	}
	if !stringSliceContains(result.Runtime.ComposeFiles, overridePath) {
		t.Fatalf("runtime compose files should include label override: %#v", result.Runtime.ComposeFiles)
	}
	if !containsCommand(runner.commands, overridePath+" -p") {
		t.Fatalf("docker compose commands should include label override file: %#v", runner.commands)
	}
}

func TestStartRuntimeReadmeCommandHonorsExplicitProjectAndFiles(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("`docker compose -p readmeproj -f custom.yml up --build`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "custom.yml"), []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedDockerRunner{}
	cfg := config.Default().Docker

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: filepath.Join(root, "artifacts"),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	if result.Runtime.ComposeProject != "readmeproj" || result.RuntimeSummary.ComposeProjectSource != "readme_project_flag" {
		t.Fatalf("README explicit project should be preserved: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if !stringSliceContains(result.Runtime.ComposeFiles, "custom.yml") || result.RuntimeSummary.Pull.Status != "skipped" || result.RuntimeSummary.Build.Status != "skipped" {
		t.Fatalf("README runtime should record command files and skipped prep steps: runtime=%#v summary=%#v", result.Runtime, result.RuntimeSummary)
	}
	if containsCommand(runner.commands, "-p p2rqa_") || !containsCommand(runner.commands, "-p readmeproj") || !containsCommand(runner.commands, "-f custom.yml") {
		t.Fatalf("docker commands should preserve README project and files: %#v", runner.commands)
	}
}

func TestStartRuntimeReadmeCommandModeAddsManagedLabelOverride(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("`docker compose -f custom.yml up`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "custom.yml"), []byte("services:\n  web:\n    image: nginx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactRoot := t.TempDir()
	runner := &scriptedDockerRunner{configOutput: "services:\n  web:\n    image: nginx\n"}
	cfg := config.Default().Docker

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: artifactRoot,
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Labels: map[string]string{
			"managed_by": "p2rqa",
		},
		Timeouts: dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("start runtime failed: %v", err)
	}
	overridePath := filepath.Join(artifactRoot, "runtime_labels.compose.yml")
	content, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "managed_by: p2rqa") {
		t.Fatalf("readme label override missing managed label:\n%s", content)
	}
	if !result.RuntimeSummary.ReadmeCommandMode {
		t.Fatalf("readme runtime summary should include label override: %#v", result.RuntimeSummary)
	}
	if !stringSliceContains(result.Runtime.ComposeFiles, "custom.yml") || !stringSliceContains(result.Runtime.ComposeFiles, overridePath) {
		t.Fatalf("readme runtime should preserve original and label compose files: %#v", result.Runtime.ComposeFiles)
	}
	if !containsCommand(runner.commands, "custom.yml") || !containsCommand(runner.commands, overridePath+" up") {
		t.Fatalf("readme compose commands should include custom and label override files: %#v", runner.commands)
	}
}

func TestStartRuntimeDiagnosesAddressPoolExhaustionAndCleansFailure(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{
		upErr:    true,
		upStderr: "failed to create network p2rqa_default: Error response from daemon: invalid pool request: Pool overlaps with other one on this address space",
	}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	var log strings.Builder
	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Log:          &log,
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected runtime startup error")
	}
	var runtimeErr *dockermgr.StartRuntimeError
	if !errors.As(err, &runtimeErr) {
		t.Fatalf("error type = %T, want StartRuntimeError", err)
	}
	if runtimeErr.Category != "docker_address_pool_exhausted" || result.RuntimeSummary.RuntimeErrorCategory != "docker_address_pool_exhausted" {
		t.Fatalf("category not classified: err=%#v summary=%#v", runtimeErr, result.RuntimeSummary)
	}
	if result.Runtime.HasCleanupTarget() {
		t.Fatalf("failed runtime should not be returned as a live runtime after cleanup: %#v", result.Runtime)
	}
	if !result.FailureRuntime.HasCleanupTarget() {
		t.Fatalf("failure cleanup target should be recorded: %#v", result.FailureRuntime)
	}
	diagnostics := result.RuntimeSummary.FailureDiagnostics
	if diagnostics == nil || diagnostics.Cleanup == nil || diagnostics.Cleanup.Status != "ok" {
		t.Fatalf("failure cleanup not recorded: %#v", diagnostics)
	}
	if !containsCommand(runner.commands, " down ") {
		t.Fatalf("failure path should run compose down: %#v", runner.commands)
	}
	if !containsDiagnostic(diagnostics.Commands, "docker_managed_networks") || !strings.Contains(log.String(), "docker_system_df") {
		t.Fatalf("diagnostics should include Docker resource snapshots: diagnostics=%#v log=%s", diagnostics.Commands, log.String())
	}
}

func TestStartRuntimeDiagnosesExitedServiceWithLogsAndInspect(t *testing.T) {
	repo := writeComposeProject(t, t.TempDir())
	runner := &scriptedDockerRunner{
		upErr:        true,
		upStderr:     "dependency failed to start: container p2rqa_backend_1 exited (1)",
		psQAllOutput: "backend-container\n",
		inspectOutput: `[
  {"Name":"/p2rqa_backend_1","State":{"Status":"exited","ExitCode":1,"Error":""}}
]`,
		composeLogsOutput: "backend-1  Traceback: DATABASE_URL is required\n",
	}
	cfg := config.Default().Docker
	cfg.PullPolicy = "skip"
	cfg.BuildMirrors.Enabled = false

	result, err := (dockermgr.Service{Exec: runner, Config: cfg}).StartRuntime(context.Background(), dockermgr.StartRuntimeRequest{
		RepoPath:     repo,
		ArtifactRoot: t.TempDir(),
		TaskID:       "TASK-1",
		RunID:        "run-1",
		Timeouts:     dockermgr.RuntimeTimeouts{Health: time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected runtime startup error")
	}
	var runtimeErr *dockermgr.StartRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Category != "compose_service_failed" {
		t.Fatalf("service failure not classified: err=%#v", err)
	}
	diagnostics := result.RuntimeSummary.FailureDiagnostics
	if diagnostics == nil {
		t.Fatal("expected failure diagnostics")
	}
	logs := diagnosticByName(diagnostics.Commands, "compose_logs_tail")
	if !strings.Contains(logs.Stdout, "DATABASE_URL is required") {
		t.Fatalf("compose logs not captured in diagnostics: %#v", logs)
	}
	inspect := diagnosticByName(diagnostics.Commands, "container_inspect")
	if !strings.Contains(inspect.Stdout, `"ExitCode":1`) {
		t.Fatalf("container inspect not captured in diagnostics: %#v", inspect)
	}
}

func TestDaemonMirrorsApplyRestoreAndInvalidJSON(t *testing.T) {
	root := t.TempDir()
	daemonPath := filepath.Join(root, "daemon.json")
	backupDir := filepath.Join(root, "backups")
	scanPath := filepath.Join(root, "scan")
	if err := os.WriteFile(daemonPath, []byte(`{"debug":true,"registry-mirrors":["https://old.example"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = scanPath
	cfg.Docker.DaemonMirrors.DaemonJSON = daemonPath
	cfg.Docker.DaemonMirrors.BackupDir = backupDir
	cfg.Docker.DaemonMirrors.RegistryMirrors = []string{"https://mirror.example"}
	cfg.Docker.DaemonMirrors.Enabled = true
	cfg.Docker.DaemonMirrors.RequireManualApply = true

	if _, err := dockermgr.ApplyDaemonMirrors(cfg, false); err == nil {
		t.Fatal("apply without --yes should fail")
	}
	applied, err := dockermgr.ApplyDaemonMirrors(cfg, true)
	if err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if !applied.OK || applied.BackupPath == "" || !applied.RestartRequired {
		t.Fatalf("unexpected apply summary: %#v", applied)
	}
	content, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"debug": true`) || !strings.Contains(string(content), "https://mirror.example") {
		t.Fatalf("daemon merge should preserve unknown fields and apply mirrors:\n%s", content)
	}
	if _, err := dockermgr.RestoreDaemonMirrors(cfg, applied.BackupPath, true); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	restored, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restored), "https://old.example") {
		t.Fatalf("restore did not restore backup:\n%s", restored)
	}

	if err := os.WriteFile(daemonPath, []byte(`{"registry-mirrors":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := dockermgr.ApplyDaemonMirrors(cfg, true); err == nil {
		t.Fatal("invalid daemon JSON should fail")
	}
	afterInvalid, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterInvalid) != `{"registry-mirrors":` {
		t.Fatalf("invalid daemon JSON should not be overwritten: %s", afterInvalid)
	}
}

func TestDaemonMirrorsPlanWritesManualApplyFileWithoutDaemonWrite(t *testing.T) {
	root := t.TempDir()
	daemonPath := filepath.Join(root, "daemon.json")
	backupDir := filepath.Join(root, "backups")
	scanPath := filepath.Join(root, "scan")
	original := `{"debug":true,"registry-mirrors":["https://old.example"]}`
	if err := os.WriteFile(daemonPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ScanPath = scanPath
	cfg.Docker.DaemonMirrors.DaemonJSON = daemonPath
	cfg.Docker.DaemonMirrors.BackupDir = backupDir
	cfg.Docker.DaemonMirrors.RegistryMirrors = []string{"https://mirror.example"}
	cfg.Docker.DaemonMirrors.Enabled = true
	cfg.Docker.DaemonMirrors.RequireManualApply = true

	planned, err := dockermgr.PlanDaemonMirrors(cfg)
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if !planned.OK || planned.ManualApplyPath == "" || planned.ManualApplyCommand == "" || !planned.RestartRequired {
		t.Fatalf("unexpected manual apply summary: %#v", planned)
	}
	current, err := os.ReadFile(daemonPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != original {
		t.Fatalf("manual plan should not write daemon.json:\n%s", current)
	}
	proposed, err := os.ReadFile(planned.ManualApplyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(proposed), "https://mirror.example") || !strings.Contains(string(proposed), `"debug": true`) {
		t.Fatalf("manual apply file should preserve fields and include desired mirror:\n%s", proposed)
	}
	summary, err := os.ReadFile(dockermgr.DaemonMirrorSummaryPath(scanPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), `"operation": "manual_apply"`) || !strings.Contains(string(summary), `"manual_apply_path"`) {
		t.Fatalf("summary should record manual apply plan:\n%s", summary)
	}
}

func TestRunGCDryRunAndExplicitDelete(t *testing.T) {
	cfg := config.Default().Docker
	dryRunner := &gcRunner{}
	dry, err := dockermgr.RunGC(context.Background(), dockermgr.GCRunRequest{
		ScanPath: t.TempDir(),
		Config:   cfg,
		Exec:     dryRunner,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if !dry.OK || !dry.DryRun {
		t.Fatalf("unexpected dry run summary: %#v", dry)
	}
	if containsCommand(dryRunner.commands, " rm ") || containsCommand(dryRunner.commands, "network rm") {
		t.Fatalf("dry run should not delete: %#v", dryRunner.commands)
	}
	for _, command := range dry.Commands {
		if strings.Contains(command, "system prune") || strings.Contains(command, "--volumes") {
			t.Fatalf("gc should not use dangerous prune command: %s", command)
		}
	}

	runRunner := &gcRunner{}
	run, err := dockermgr.RunGC(context.Background(), dockermgr.GCRunRequest{
		ScanPath: t.TempDir(),
		Config:   cfg,
		Exec:     runRunner,
		Yes:      true,
	})
	if err != nil {
		t.Fatalf("gc run failed: %v", err)
	}
	if !run.OK || !containsCommand(runRunner.commands, " rm abc123") || !containsCommand(runRunner.commands, "network rm net123") {
		t.Fatalf("gc run should delete enumerated p2r resources: summary=%#v commands=%#v", run, runRunner.commands)
	}
	if !containsCommand(runRunner.commands, " rm compose123") || !containsCommand(runRunner.commands, "network rm net456") {
		t.Fatalf("gc run should also delete compose-project-prefixed resources: commands=%#v", runRunner.commands)
	}
	if containsCommand(runRunner.commands, "other123") || containsCommand(runRunner.commands, "othernet") {
		t.Fatalf("gc run must not delete non-p2r compose resources: %#v", runRunner.commands)
	}
}

func TestRunGCGlobalScopeRequiresAllowGlobal(t *testing.T) {
	cfg := config.Default().Docker
	cfg.GC.P2ROnly = false
	summary, err := dockermgr.RunGC(context.Background(), dockermgr.GCRunRequest{
		ScanPath: t.TempDir(),
		Config:   cfg,
		Exec:     &gcRunner{},
		Yes:      true,
	})
	if err == nil {
		t.Fatal("global GC without allow-global should fail")
	}
	if summary.OK || !strings.Contains(summary.Error, "--allow-global") {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

type scriptedDockerRunner struct {
	pullErr                         bool
	upErr                           bool
	upStderr                        string
	configOutput                    string
	streamChunks                    []string
	psQAllOutput                    string
	inspectOutput                   string
	composeLogsOutput               string
	rejectMirrorBuildOutsideContext bool
	commands                        []string
}

func (r *scriptedDockerRunner) LookPath(name string) (string, error) {
	if name == "docker" {
		return "docker", nil
	}
	return "", errors.New("not found")
}

func (r *scriptedDockerRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	return r.result(name, args...)
}

func (r *scriptedDockerRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	result := r.result(name, args...)
	if writer != nil {
		if len(r.streamChunks) > 0 {
			for _, chunk := range r.streamChunks {
				_, _ = writer.Write([]byte(chunk))
			}
		} else {
			_, _ = writer.Write([]byte(result.Stdout + result.Stderr))
		}
	}
	if onOutput != nil && strings.TrimSpace(result.Stdout) != "" {
		onOutput(strings.TrimSpace(result.Stdout), "stdout")
	}
	return result
}

func (r *scriptedDockerRunner) result(name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	if strings.Contains(command, " pull ") && r.pullErr {
		return executor.Result{Command: command, ExitCode: 1, Stderr: "pull failed", Err: errors.New("pull failed")}
	}
	if strings.Contains(command, " up -d") && r.upErr {
		stderr := r.upStderr
		if strings.TrimSpace(stderr) == "" {
			stderr = "compose up failed"
		}
		return executor.Result{Command: command, ExitCode: 1, Stderr: stderr, Err: errors.New("compose up failed")}
	}
	if strings.Contains(command, " build") && strings.Contains(command, "compose.mirror.override.yml") && r.rejectMirrorBuildOutsideContext {
		return executor.Result{Command: command, ExitCode: 1, Stderr: "failed to read dockerfile: forbidden path outside the build context", Err: errors.New("build failed")}
	}
	if strings.Contains(command, " ps --all -q") {
		return executor.Result{Command: command, Stdout: r.psQAllOutput}
	}
	if strings.Contains(command, " ps -q") {
		return executor.Result{Command: command}
	}
	if strings.Contains(command, " logs ") {
		return executor.Result{Command: command, Stdout: r.composeLogsOutput}
	}
	if strings.HasPrefix(command, "docker inspect ") {
		return executor.Result{Command: command, Stdout: r.inspectOutput}
	}
	if strings.Contains(command, " config") && strings.TrimSpace(r.configOutput) != "" {
		return executor.Result{Command: command, Stdout: r.configOutput}
	}
	return executor.Result{Command: command, Stdout: command + " ok\n"}
}

type gcRunner struct {
	commands []string
}

func (r *gcRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (r *gcRunner) Run(ctx context.Context, timeout time.Duration, dir string, env []string, name string, args ...string) executor.Result {
	command := strings.Join(append([]string{name}, args...), " ")
	r.commands = append(r.commands, command)
	switch {
	case strings.Contains(command, "ps -a") && strings.Contains(command, "label=managed_by=p2rqa"):
		return executor.Result{Command: command, Stdout: `{"ID":"abc123","Names":"p2rqa_web_1","Labels":"managed_by=p2rqa"}` + "\n"}
	case strings.Contains(command, "ps -a") && strings.Contains(command, "label=com.docker.compose.project"):
		return executor.Result{Command: command, Stdout: strings.Join([]string{
			`{"ID":"compose123","Names":"p2rqa_api_1","Labels":"com.docker.compose.project=p2rqa_task_run"}`,
			`{"ID":"other123","Names":"other_api_1","Labels":"com.docker.compose.project=other_task_run"}`,
		}, "\n") + "\n"}
	case strings.Contains(command, "network ls") && strings.Contains(command, "label=managed_by=p2rqa"):
		return executor.Result{Command: command, Stdout: `{"ID":"net123","Name":"p2rqa_default","Labels":"managed_by=p2rqa"}` + "\n"}
	case strings.Contains(command, "network ls") && strings.Contains(command, "label=com.docker.compose.project"):
		return executor.Result{Command: command, Stdout: strings.Join([]string{
			`{"ID":"net456","Name":"p2rqa_task_run_default","Labels":"com.docker.compose.project=p2rqa_task_run"}`,
			`{"ID":"othernet","Name":"other_default","Labels":"com.docker.compose.project=other_task_run"}`,
		}, "\n") + "\n"}
	default:
		return executor.Result{Command: command, Stdout: "ok\n"}
	}
}

func (r *gcRunner) RunStreamingWithOutput(ctx context.Context, timeout time.Duration, dir string, env []string, writer io.Writer, onOutput executor.OutputCallback, name string, args ...string) executor.Result {
	return r.Run(ctx, timeout, dir, env, name, args...)
}

func writeComposeProject(t *testing.T, root string) string {
	t.Helper()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "services:\n  web:\n    image: nginx\n"
	if err := os.WriteFile(filepath.Join(repo, "docker-compose.yml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func containsCommand(commands []string, needle string) bool {
	for _, command := range commands {
		if strings.Contains(command, needle) {
			return true
		}
	}
	return false
}

func containsExplicitComposeBuildCommand(commands []string) bool {
	for _, command := range commands {
		if strings.HasSuffix(command, " build") || strings.Contains(command, " build ") {
			return true
		}
	}
	return false
}

func stringSliceContains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func containsDiagnostic(commands []dockermgr.RuntimeDiagnosticCommand, name string) bool {
	return diagnosticByName(commands, name).Name != ""
}

func diagnosticByName(commands []dockermgr.RuntimeDiagnosticCommand, name string) dockermgr.RuntimeDiagnosticCommand {
	for _, command := range commands {
		if command.Name == name {
			return command
		}
	}
	return dockermgr.RuntimeDiagnosticCommand{}
}

func testDebianMultiarchLibDir() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64-linux-gnu"
	case "arm64":
		return "aarch64-linux-gnu"
	case "arm":
		return "arm-linux-gnueabihf"
	case "386":
		return "i386-linux-gnu"
	case "ppc64le":
		return "powerpc64le-linux-gnu"
	case "s390x":
		return "s390x-linux-gnu"
	default:
		return ""
	}
}
