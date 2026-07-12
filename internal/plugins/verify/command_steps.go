package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	harborverify "github.com/purplevoid/harbor-factory/internal/harbor/verify"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const (
	DockerBuildPluginID   = "harborfactory.docker_build"
	DockerBuildKind       = "harborfactory.docker_build"
	InitialVerifyPluginID = "harborfactory.initial_verify"
	InitialVerifyKind     = "harborfactory.initial_verify"
	OracleVerifyPluginID  = "harborfactory.oracle_verify"
	OracleVerifyKind      = "harborfactory.oracle_verify"
)

type DockerBuildPlugin struct{}
type InitialVerifyPlugin struct{}
type OracleVerifyPlugin struct{}

func (DockerBuildPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: DockerBuildPluginID, Version: "1.0.0", Kinds: []string{DockerBuildKind}}
}

func (InitialVerifyPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: InitialVerifyPluginID, Version: "1.0.0", Kinds: []string{InitialVerifyKind}}
}

func (OracleVerifyPlugin) Manifest() workflow.PluginManifest {
	return workflow.PluginManifest{ID: OracleVerifyPluginID, Version: "1.0.0", Kinds: []string{OracleVerifyKind}}
}

func (DockerBuildPlugin) Validate(spec workflow.NodeSpec) error   { return requireTaskDir(spec) }
func (InitialVerifyPlugin) Validate(spec workflow.NodeSpec) error { return requireTaskDir(spec) }
func (OracleVerifyPlugin) Validate(spec workflow.NodeSpec) error  { return requireTaskDir(spec) }

func requireTaskDir(spec workflow.NodeSpec) error {
	return pluginutil.RequiredString(spec, "task_dir")
}

func (DockerBuildPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	environment, err := environmentFor(req)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	run, runErr := executeCommand(ctx, req, harborverify.DockerBuildCommand(environment))
	run.Passed = runErr == nil && run.ExitCode == 0 && !run.Timeout
	refs, storeErr := storeCommand(ctx, req, &run)
	result := workflow.NodeResult{Artifacts: refs}
	if storeErr != nil {
		return result, fmt.Errorf("store docker build evidence: %w", storeErr)
	}
	if !run.Passed {
		return result, commandFailure(run, runErr, "docker build")
	}
	return result, nil
}

func (InitialVerifyPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	environment, err := environmentFor(req)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	run, runErr := executeCommand(ctx, req, harborverify.InitialVerifyCommand(environment))
	run.Passed = harborverify.InitialVerificationExposesIssue(run)
	refs, storeErr := storeCommand(ctx, req, &run)
	result := workflow.NodeResult{Artifacts: refs}
	if storeErr != nil {
		return result, fmt.Errorf("store initial verification evidence: %w", storeErr)
	}
	if !run.Passed {
		return result, commandFailure(run, runErr, "initial verification")
	}
	return result, nil
}

func (OracleVerifyPlugin) Execute(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	environment, err := environmentFor(req)
	if err != nil {
		return workflow.NodeResult{}, err
	}
	run, runErr := executeCommand(ctx, req, harborverify.OracleVerifyCommand(environment))
	run.Passed = runErr == nil && run.ExitCode == 0 && !run.Timeout
	refs, storeErr := storeCommand(ctx, req, &run)
	result := workflow.NodeResult{Artifacts: refs}
	if storeErr != nil {
		return result, fmt.Errorf("store oracle verification evidence: %w", storeErr)
	}
	if !run.Passed {
		return result, commandFailure(run, runErr, "oracle verification")
	}
	reportRefs, reportErr := storeVerifyReport(ctx, req, environment, run)
	if reportErr != nil {
		return result, reportErr
	}
	result.Artifacts = append(result.Artifacts, reportRefs...)
	return result, nil
}

func storeVerifyReport(ctx context.Context, req workflow.NodeRequest, environment harborverify.Environment, oracle domain.CommandRun) ([]workflow.ArtifactRef, error) {
	build, err := priorCommand(ctx, req, "docker_build")
	if err != nil {
		return nil, err
	}
	initial, err := priorCommand(ctx, req, "initial_verify")
	if err != nil {
		return nil, err
	}
	commands := []*domain.CommandRun{&build, &initial, &oracle}
	var evidence []workflow.ArtifactRef
	for _, command := range commands {
		base := filepath.ToSlash(filepath.Join("phase2", "artifacts", "verify", "command_logs", safeName(command.Name)))
		stdoutRef, putErr := req.Store.PutText(ctx, base+"/stdout.log", "command_stdout", req.Spec.ID, command.Stdout)
		if putErr != nil {
			return evidence, fmt.Errorf("store %s verification stdout: %w", command.Name, putErr)
		}
		stderrRef, putErr := req.Store.PutText(ctx, base+"/stderr.log", "command_stderr", req.Spec.ID, command.Stderr)
		if putErr != nil {
			return append(evidence, stdoutRef), fmt.Errorf("store %s verification stderr: %w", command.Name, putErr)
		}
		command.StdoutPath = stdoutRef.Path
		command.StderrPath = stderrRef.Path
		evidence = append(evidence, stdoutRef, stderrRef)
	}
	digest, err := harborrun.ComputeTaskDigest(environment.TaskDir)
	if err != nil {
		return evidence, fmt.Errorf("compute verified task digest: %w", err)
	}
	report := domain.VerifyReport{
		SchemaVersion: "harbor.verify_report.v1", TaskDir: environment.TaskDir, TaskDigest: digest,
		ImageTag: environment.ImageTag, DockerBuild: &build, InitialVerify: &initial,
		InitialExposesIssue: initial.Passed, OracleVerify: &oracle,
		CommandLogs: []domain.CommandRun{build, initial, oracle}, Passed: build.Passed && initial.Passed && oracle.Passed, CreatedAt: time.Now().UTC(),
	}
	ref, err := req.Store.PutJSON(ctx, "phase2/artifacts/verify/verify_report.json", "verify_report", req.Spec.ID, report)
	if err != nil {
		return evidence, err
	}
	return append(evidence, ref), nil
}

func priorCommand(ctx context.Context, req workflow.NodeRequest, nodeID string) (domain.CommandRun, error) {
	prior, ok := req.Prior[nodeID]
	if !ok {
		return domain.CommandRun{}, fmt.Errorf("%s evidence is required", nodeID)
	}
	for _, ref := range prior.Artifacts {
		if !strings.EqualFold(ref.Type, "command_run") {
			continue
		}
		reader, _, err := req.Store.Get(ctx, ref)
		if err != nil {
			return domain.CommandRun{}, err
		}
		var run domain.CommandRun
		decodeErr := json.NewDecoder(reader).Decode(&run)
		closeErr := reader.Close()
		if decodeErr != nil {
			return domain.CommandRun{}, fmt.Errorf("decode %s evidence: %w", nodeID, decodeErr)
		}
		if closeErr != nil {
			return domain.CommandRun{}, closeErr
		}
		return run, nil
	}
	return domain.CommandRun{}, fmt.Errorf("%s command evidence is missing", nodeID)
}
