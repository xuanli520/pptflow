package infra

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/plugins/pluginutil"
	"github.com/purplevoid/harbor-factory/internal/workflow"
)

const gatePreviewLimit = 8000

// buildGateEvidence derives the review surface from the exact canonical
// artifacts resolved by Engine for this gate. Configured items are only
// additive; they cannot replace deterministic evidence checks.
func buildGateEvidence(ctx context.Context, req workflow.NodeRequest) ([]domain.ChecklistItem, []domain.ArtifactPreview, error) {
	checklist := configuredGateChecklist(req)
	artifacts := configuredGateArtifacts(req)
	seenItems := make(map[string]bool, len(checklist))
	seenArtifacts := make(map[string]bool, len(artifacts))
	for _, item := range checklist {
		seenItems[item.ID] = true
	}
	for _, artifact := range artifacts {
		seenArtifacts[artifact.Path] = true
	}

	for _, ref := range req.Inputs {
		reader, meta, err := req.Store.Get(ctx, ref)
		if err != nil {
			return nil, nil, fmt.Errorf("read canonical artifact %s: %w", ref.Name, err)
		}
		raw, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			return nil, nil, fmt.Errorf("read canonical artifact %s: %w", ref.Name, readErr)
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}

		items, err := checklistForArtifact(ref, raw, req)
		if err != nil {
			return nil, nil, err
		}
		for _, item := range items {
			if item.ID == "" || seenItems[item.ID] {
				continue
			}
			seenItems[item.ID] = true
			checklist = append(checklist, item)
		}
		if !seenArtifacts[meta.Path] {
			if ref.Type == "pass4_screenshot" {
				artifacts = append(artifacts, domain.ArtifactPreview{Name: ref.Name, Path: meta.Path, Content: "Headless Harbor pass@4 PNG evidence"})
				seenArtifacts[meta.Path] = true
				continue
			}
			preview := raw
			if len(preview) > gatePreviewLimit {
				preview = preview[:gatePreviewLimit]
			}
			content := commandlog.RedactText(string(preview))
			if len(raw) > gatePreviewLimit {
				content = strings.TrimSuffix(content, "\n") + "\n... truncated ..."
			}
			artifacts = append(artifacts, domain.ArtifactPreview{Name: ref.Name, Path: meta.Path, Content: content})
			seenArtifacts[meta.Path] = true
		}
	}
	return checklist, artifacts, nil
}

func checklistForArtifact(ref workflow.ArtifactRef, raw []byte, req workflow.NodeRequest) ([]domain.ChecklistItem, error) {
	contentRequired := ref.Type != "command_stdout" && ref.Type != "command_stderr"
	present := domain.ChecklistItem{
		ID:       "artifact_" + checklistID(ref.Type, ref.Name),
		Label:    fmt.Sprintf("canonical %s artifact is present and digest-verified", defaultString(ref.Type, ref.Name)),
		Critical: true,
		Passed:   strings.TrimSpace(ref.SHA256) != "" && (!contentRequired || len(strings.TrimSpace(string(raw))) > 0),
	}
	items := []domain.ChecklistItem{present}
	switch ref.Type {
	case "repo_analysis":
		var report domain.RepoAnalysis
		if err := decodeGateJSON(ref, raw, &report); err != nil {
			return nil, err
		}
		items = append(items,
			gateItem("repo_analysis_identity", "repository analysis is bound to a repository and commit", report.RepoURL != "" && report.CommitSHA != ""),
			advisoryGateItem("repo_analysis_toolchain", "repository language, build system, and test framework are identified", report.Language != "" && report.BuildSystem != "" && report.TestFramework != ""),
			advisoryGateItem("repo_analysis_task_areas", "repository analysis contains candidate engineering task areas", len(report.PotentialTaskAreas) > 0),
		)
	case "task_proposal":
		var proposal domain.TaskProposal
		if err := decodeGateJSON(ref, raw, &proposal); err != nil {
			return nil, err
		}
		items = append(items,
			gateItem("task_proposal_identity", "task name and one-line engineering scenario are present", proposal.TaskName != "" && proposal.OneLineDescription != ""),
			gateItem("task_proposal_source", "task proposal is bound to a public repository commit", proposal.GitHubLink != "" && proposal.CommitSHA != ""),
			gateItem("task_proposal_metadata", "code language, task type, and application are present", proposal.CodeLang != "" && proposal.TaskType != "" && proposal.Application != ""),
			gateItem("task_proposal_difficulty", "difficulty rationale and positive AHT are present", proposal.DifficultyRationale != "" && proposal.EstimatedAHTMinutes > 0),
		)
	case "lint_report":
		var report domain.LintReport
		if err := decodeGateJSON(ref, raw, &report); err != nil {
			return nil, err
		}
		items = append(items, gateItem("lint_overall", "deterministic CodeEdge lint passes", report.Passed))
		for _, check := range report.Checks {
			items = append(items, domain.ChecklistItem{ID: "lint_" + checklistID(check.ID, check.Message), Label: check.Message, Critical: check.Status != domain.CheckWarn, Passed: check.Status != domain.CheckFail})
		}
	case "verify_report":
		var report domain.VerifyReport
		if err := decodeGateJSON(ref, raw, &report); err != nil {
			return nil, err
		}
		items = append(items,
			gateItem("verify_docker_build", "Docker environment builds successfully", report.DockerBuild != nil && report.DockerBuild.Passed),
			gateItem("verify_initial_failure", "initial verifier run exposes the intended issue", report.InitialVerify != nil && report.InitialExposesIssue && report.InitialVerify.Passed),
			gateItem("verify_oracle", "oracle solution passes the verifier", report.OracleVerify != nil && report.OracleVerify.Passed),
			gateItem("verify_overall", "runtime verification passes and is bound to a task digest", report.Passed && report.TaskDigest != ""),
		)
	case "quality_report":
		var report domain.QualityReport
		if err := decodeGateJSON(ref, raw, &report); err != nil {
			return nil, err
		}
		items = append(items, gateItem("quality_overall", "semantic quality review passes", report.OverallPass))
		keys := make([]string, 0, len(report.Checks))
		for key := range report.Checks {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			check := report.Checks[key]
			critical := strings.EqualFold(check.Severity, "error")
			items = append(items, domain.ChecklistItem{ID: "quality_" + checklistID(key, check.Detail), Label: check.Detail, Critical: critical, Passed: check.Passed || !critical})
		}
	case "similarity_report":
		var report domain.SimilarityReport
		if err := decodeGateJSON(ref, raw, &report); err != nil {
			return nil, err
		}
		items = append(items,
			gateItem("similarity_overall", fmt.Sprintf("max similarity %.3f is below threshold %.3f", report.MaxScore, report.Threshold), report.OverallPass),
			gateItem("similarity_sources", "at least one configured similarity source completed with reproducible evidence", len(report.SuccessfulSources) > 0 && len(report.SourceEvidence) > 0),
		)
	case "trial_result":
		var result domain.TrialResult
		if err := decodeGateJSON(ref, raw, &result); err != nil {
			return nil, err
		}
		qwen := strings.Contains(strings.ToLower(ref.Producer+" "+ref.Name), "qwen")
		expectedModel := pluginutil.String(req, "opus_model")
		prefix := "opus"
		if qwen {
			expectedModel = pluginutil.String(req, "qwen_model")
			prefix = "qwen"
		}
		failures := harborrun.ValidateForCodeEdgeWithOptions(result, harborrun.ValidationOptions{
			Qwen: qwen, ExpectedModel: expectedModel, ExpectedAgent: pluginutil.String(req, "harbor_agent"),
			TaskDir: pluginutil.String(req, "task_dir"), RequireRuns: true, RequireTaskDigest: true, RequireCommandRun: true,
		})
		items = append(items, gateItem(prefix+"_strict_result", prefix+" Harbor evidence has exact model, four trials, task digest, and command provenance", len(failures) == 0))
		for index, failure := range failures {
			items = append(items, domain.ChecklistItem{ID: fmt.Sprintf("%s_failure_%02d", prefix, index+1), Label: failure, Critical: true, Passed: false})
		}
		screenshot := strings.TrimSpace(result.Screenshot)
		if configured := pluginutil.String(req, prefix+"_screenshot"); configured != "" {
			screenshot = configured
		} else if screenshot != "" && !filepath.IsAbs(screenshot) {
			screenshot = filepath.Join(filepath.Dir(ref.Path), screenshot)
		}
		items = append(items, gateItem(prefix+"_screenshot", prefix+" pass@4 screenshot is readable", readableRegularFile(screenshot)))
	}
	return items, nil
}

func decodeGateJSON(ref workflow.ArtifactRef, raw []byte, target any) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode canonical %s artifact %s: %w", ref.Type, ref.Name, err)
	}
	return nil
}

func gateItem(id, label string, passed bool) domain.ChecklistItem {
	return domain.ChecklistItem{ID: id, Label: label, Critical: true, Passed: passed}
}

func advisoryGateItem(id, label string, passed bool) domain.ChecklistItem {
	return domain.ChecklistItem{ID: id, Label: label, Critical: false, Passed: passed}
}

func readableRegularFile(path string) bool {
	info, err := os.Stat(strings.TrimSpace(path))
	return err == nil && info.Mode().IsRegular()
}

func checklistID(primary, fallback string) string {
	value := strings.ToLower(strings.TrimSpace(primary))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(fallback))
	}
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case out.Len() > 0 && !strings.HasSuffix(out.String(), "_"):
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_")
}
