package sanitize

import (
	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

func Text(value string) string {
	return commandlog.RedactText(value)
}

func StringSlice(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, Text(value))
	}
	return out
}

func ArtifactPreview(artifact domain.ArtifactPreview) domain.ArtifactPreview {
	artifact.Name = Text(artifact.Name)
	artifact.Path = Text(artifact.Path)
	artifact.Content = Text(artifact.Content)
	return artifact
}

func ArtifactPreviews(artifacts []domain.ArtifactPreview) []domain.ArtifactPreview {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]domain.ArtifactPreview, len(artifacts))
	for i, artifact := range artifacts {
		out[i] = ArtifactPreview(artifact)
	}
	return out
}

func ChecklistItem(item domain.ChecklistItem) domain.ChecklistItem {
	item.ID = Text(item.ID)
	item.Label = Text(item.Label)
	return item
}

func ChecklistItems(items []domain.ChecklistItem) []domain.ChecklistItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]domain.ChecklistItem, len(items))
	for i, item := range items {
		out[i] = ChecklistItem(item)
	}
	return out
}

func GateRequest(request domain.GateRequest) domain.GateRequest {
	request.RequestID = Text(request.RequestID)
	request.GateID = Text(request.GateID)
	request.GateName = Text(request.GateName)
	request.NodeID = Text(request.NodeID)
	request.Message = Text(request.Message)
	request.Checklist = ChecklistItems(request.Checklist)
	request.Artifacts = ArtifactPreviews(request.Artifacts)
	return request
}

func GateDecision(decision domain.GateDecision) domain.GateDecision {
	decision.RequestID = Text(decision.RequestID)
	decision.GateID = Text(decision.GateID)
	decision.Action = Text(decision.Action)
	decision.Notes = Text(decision.Notes)
	if len(decision.EditedFiles) > 0 {
		edited := make(map[string]string, len(decision.EditedFiles))
		for path, summary := range decision.EditedFiles {
			edited[Text(path)] = Text(summary)
		}
		decision.EditedFiles = edited
	}
	return decision
}

func RunnerEvent(event domain.RunnerEvent) domain.RunnerEvent {
	event.RunID = Text(event.RunID)
	event.Type = Text(event.Type)
	event.NodeID = Text(event.NodeID)
	event.Status = Text(event.Status)
	event.Message = Text(event.Message)
	event.Path = Text(event.Path)
	event.Artifacts = ArtifactPreviews(event.Artifacts)
	event.Logs = ArtifactPreviews(event.Logs)
	if event.Gate != nil {
		gate := GateRequest(*event.Gate)
		event.Gate = &gate
	}
	return event
}

func RunnerEvents(events []domain.RunnerEvent) []domain.RunnerEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]domain.RunnerEvent, len(events))
	for i, event := range events {
		out[i] = RunnerEvent(event)
	}
	return out
}

func CommandRun(run domain.CommandRun) domain.CommandRun {
	run.Name = Text(run.Name)
	run.Command = Text(run.Command)
	run.Argv = commandlog.RedactArgv(run.Argv)
	run.Dir = Text(run.Dir)
	run.Env = commandlog.RedactEnv(run.Env)
	run.Stdout = Text(run.Stdout)
	run.Stderr = Text(run.Stderr)
	run.StdoutPath = Text(run.StdoutPath)
	run.StderrPath = Text(run.StderrPath)
	run.FailureClass = Text(run.FailureClass)
	return run
}

func commandRunPtr(run *domain.CommandRun) *domain.CommandRun {
	if run == nil {
		return nil
	}
	copy := CommandRun(*run)
	return &copy
}

func commandRuns(runs []domain.CommandRun) []domain.CommandRun {
	if len(runs) == 0 {
		return nil
	}
	out := make([]domain.CommandRun, len(runs))
	for i, run := range runs {
		out[i] = CommandRun(run)
	}
	return out
}

func RepoPrepared(report domain.RepoPrepared) domain.RepoPrepared {
	report.RepoURL = Text(report.RepoURL)
	report.RequestedCommit = Text(report.RequestedCommit)
	report.ResolvedCommit = Text(report.ResolvedCommit)
	report.TreeHash = Text(report.TreeHash)
	report.SourcePath = Text(report.SourcePath)
	report.CommandLogs = commandRuns(report.CommandLogs)
	return report
}

func LintReport(report domain.LintReport) domain.LintReport {
	report.TaskDir = Text(report.TaskDir)
	report.RepoURL = Text(report.RepoURL)
	report.Commit = Text(report.Commit)
	for i := range report.Checks {
		report.Checks[i].ID = Text(report.Checks[i].ID)
		report.Checks[i].Message = Text(report.Checks[i].Message)
		report.Checks[i].Path = Text(report.Checks[i].Path)
	}
	return report
}

func RepoAnalysis(analysis domain.RepoAnalysis) domain.RepoAnalysis {
	analysis.SchemaVersion = Text(analysis.SchemaVersion)
	analysis.RepoURL = Text(analysis.RepoURL)
	analysis.CommitSHA = Text(analysis.CommitSHA)
	analysis.Language = Text(analysis.Language)
	analysis.LanguageVersion = Text(analysis.LanguageVersion)
	analysis.BuildSystem = Text(analysis.BuildSystem)
	analysis.TestFramework = Text(analysis.TestFramework)
	for i := range analysis.KeyModules {
		analysis.KeyModules[i].Name = Text(analysis.KeyModules[i].Name)
		analysis.KeyModules[i].Purpose = Text(analysis.KeyModules[i].Purpose)
	}
	for i := range analysis.PotentialTaskAreas {
		analysis.PotentialTaskAreas[i].Area = Text(analysis.PotentialTaskAreas[i].Area)
		analysis.PotentialTaskAreas[i].Module = Text(analysis.PotentialTaskAreas[i].Module)
		analysis.PotentialTaskAreas[i].Description = Text(analysis.PotentialTaskAreas[i].Description)
		analysis.PotentialTaskAreas[i].Difficulty = Text(analysis.PotentialTaskAreas[i].Difficulty)
		analysis.PotentialTaskAreas[i].TypeSuggestion = Text(analysis.PotentialTaskAreas[i].TypeSuggestion)
	}
	analysis.EntryPoints = StringSlice(analysis.EntryPoints)
	analysis.Dependencies = StringSlice(analysis.Dependencies)
	return analysis
}

func TaskProposal(proposal domain.TaskProposal) domain.TaskProposal {
	proposal.SchemaVersion = Text(proposal.SchemaVersion)
	proposal.TaskName = Text(proposal.TaskName)
	proposal.OneLineDescription = Text(proposal.OneLineDescription)
	proposal.CodeLang = Text(proposal.CodeLang)
	proposal.TaskType = Text(proposal.TaskType)
	proposal.Application = Text(proposal.Application)
	proposal.GitHubLink = Text(proposal.GitHubLink)
	proposal.CommitSHA = Text(proposal.CommitSHA)
	proposal.TargetFiles = StringSlice(proposal.TargetFiles)
	proposal.AffectedModules = StringSlice(proposal.AffectedModules)
	proposal.DifficultyRationale = Text(proposal.DifficultyRationale)
	proposal.BoundaryConditions = StringSlice(proposal.BoundaryConditions)
	proposal.SuggestedVerification = Text(proposal.SuggestedVerification)
	proposal.SetupCommands = StringSlice(proposal.SetupCommands)
	return proposal
}

func GenReport(report domain.GenReport) domain.GenReport {
	report.TaskDir = Text(report.TaskDir)
	report.TestsAnalysisPath = Text(report.TestsAnalysisPath)
	report.RepoAnalysisPath = Text(report.RepoAnalysisPath)
	report.TaskProposalPath = Text(report.TaskProposalPath)
	report.TaskFilesPath = Text(report.TaskFilesPath)
	report.RepoAnalysis = RepoAnalysis(report.RepoAnalysis)
	report.TaskProposal = TaskProposal(report.TaskProposal)
	return report
}

func TrialResult(result domain.TrialResult) domain.TrialResult {
	result.SchemaVersion = Text(result.SchemaVersion)
	result.Model = Text(result.Model)
	result.Agent = Text(result.Agent)
	for i := range result.Runs {
		result.Runs[i].FailureReason = Text(result.Runs[i].FailureReason)
	}
	result.ResultPath = Text(result.ResultPath)
	result.RawResultPath = Text(result.RawResultPath)
	result.RawResultSHA256 = Text(result.RawResultSHA256)
	for i := range result.RawTrialResults {
		result.RawTrialResults[i].Path = Text(result.RawTrialResults[i].Path)
		result.RawTrialResults[i].SHA256 = Text(result.RawTrialResults[i].SHA256)
	}
	result.TaskDigest = Text(result.TaskDigest)
	result.HarborTaskChecksum = Text(result.HarborTaskChecksum)
	result.TaskPath = Text(result.TaskPath)
	result.CommandRunPath = Text(result.CommandRunPath)
	result.SchemaPreflightPath = Text(result.SchemaPreflightPath)
	result.PreflightRunPath = Text(result.PreflightRunPath)
	result.PreflightResultPath = Text(result.PreflightResultPath)
	result.AgentCacheManifest = Text(result.AgentCacheManifest)
	result.RetryEvidence = Text(result.RetryEvidence)
	result.Screenshot = Text(result.Screenshot)
	return result
}

func QualityReport(report domain.QualityReport) domain.QualityReport {
	report.TaskDir = Text(report.TaskDir)
	report.TaskDigest = Text(report.TaskDigest)
	if report.Checks != nil {
		checks := make(map[string]domain.QualityCheck, len(report.Checks))
		for key, check := range report.Checks {
			check.Detail = Text(check.Detail)
			check.Severity = Text(check.Severity)
			check.Source = Text(check.Source)
			checks[Text(key)] = check
		}
		report.Checks = checks
	}
	report.Warnings = StringSlice(report.Warnings)
	report.Issues = StringSlice(report.Issues)
	report.AgentModel = Text(report.AgentModel)
	report.RequestedModel = Text(report.RequestedModel)
	report.ReasoningEffort = Text(report.ReasoningEffort)
	report.PromptFingerprint = Text(report.PromptFingerprint)
	report.RubricFingerprint = Text(report.RubricFingerprint)
	report.ReviewFingerprint = Text(report.ReviewFingerprint)
	report.AgentOutput = Text(report.AgentOutput)
	return report
}

func SimilarityReport(report domain.SimilarityReport) domain.SimilarityReport {
	report.TaskDir = Text(report.TaskDir)
	report.TaskDigest = Text(report.TaskDigest)
	report.RepoURL = Text(report.RepoURL)
	report.Sources = StringSlice(report.Sources)
	report.SuccessfulSources = StringSlice(report.SuccessfulSources)
	for i := range report.SourceEvidence {
		report.SourceEvidence[i].Source = Text(report.SourceEvidence[i].Source)
		report.SourceEvidence[i].Kind = Text(report.SourceEvidence[i].Kind)
		report.SourceEvidence[i].Path = Text(report.SourceEvidence[i].Path)
		report.SourceEvidence[i].SourceDigest = Text(report.SourceEvidence[i].SourceDigest)
	}
	for i := range report.Candidates {
		report.Candidates[i].Source = Text(report.Candidates[i].Source)
		report.Candidates[i].Title = Text(report.Candidates[i].Title)
		report.Candidates[i].Path = Text(report.Candidates[i].Path)
		report.Candidates[i].URL = Text(report.Candidates[i].URL)
		report.Candidates[i].MatchedTerms = StringSlice(report.Candidates[i].MatchedTerms)
	}
	report.Warnings = StringSlice(report.Warnings)
	report.Issues = StringSlice(report.Issues)
	return report
}

func VerifyReport(report domain.VerifyReport) domain.VerifyReport {
	report.TaskDir = Text(report.TaskDir)
	report.TaskDigest = Text(report.TaskDigest)
	report.ImageTag = Text(report.ImageTag)
	report.DockerBuild = commandRunPtr(report.DockerBuild)
	report.InitialVerify = commandRunPtr(report.InitialVerify)
	report.OracleVerify = commandRunPtr(report.OracleVerify)
	report.Cleanup = commandRunPtr(report.Cleanup)
	report.CommandLogs = commandRuns(report.CommandLogs)
	return report
}

func PackageReport(report domain.PackageReport) domain.PackageReport {
	report.TaskDir = Text(report.TaskDir)
	report.OutputZip = Text(report.OutputZip)
	report.ReportPath = Text(report.ReportPath)
	report.TaskName = Text(report.TaskName)
	return report
}

func RunSummary(summary domain.RunSummary) domain.RunSummary {
	summary.RunID = Text(summary.RunID)
	summary.PreviousRunID = Text(summary.PreviousRunID)
	summary.ReusedNodes = StringSlice(summary.ReusedNodes)
	summary.RerunNodes = StringSlice(summary.RerunNodes)
	summary.Workspace = Text(summary.Workspace)
	if summary.RepoPrepared != nil {
		report := RepoPrepared(*summary.RepoPrepared)
		summary.RepoPrepared = &report
	}
	if summary.GenReport != nil {
		report := GenReport(*summary.GenReport)
		summary.GenReport = &report
	}
	if summary.LintReport != nil {
		report := LintReport(*summary.LintReport)
		summary.LintReport = &report
	}
	if summary.VerifyReport != nil {
		report := VerifyReport(*summary.VerifyReport)
		summary.VerifyReport = &report
	}
	if summary.QualityReport != nil {
		report := QualityReport(*summary.QualityReport)
		summary.QualityReport = &report
	}
	if summary.SimilarityReport != nil {
		report := SimilarityReport(*summary.SimilarityReport)
		summary.SimilarityReport = &report
	}
	if summary.QwenResult != nil {
		result := TrialResult(*summary.QwenResult)
		summary.QwenResult = &result
	}
	if summary.OpusResult != nil {
		result := TrialResult(*summary.OpusResult)
		summary.OpusResult = &result
	}
	if summary.PackageReport != nil {
		report := PackageReport(*summary.PackageReport)
		summary.PackageReport = &report
	}
	if len(summary.GateDecisions) > 0 {
		decisions := make([]domain.GateDecision, len(summary.GateDecisions))
		for i, decision := range summary.GateDecisions {
			decisions[i] = GateDecision(decision)
		}
		summary.GateDecisions = decisions
	}
	summary.Events = RunnerEvents(summary.Events)
	summary.PersistenceErrors = StringSlice(summary.PersistenceErrors)
	summary.Status = Text(summary.Status)
	return summary
}

func RunnerOptionsSnapshot(snapshot domain.RunnerOptionsSnapshot) domain.RunnerOptionsSnapshot {
	snapshot.SchemaVersion = Text(snapshot.SchemaVersion)
	snapshot.Workspace = Text(snapshot.Workspace)
	snapshot.RepoURL = Text(snapshot.RepoURL)
	snapshot.Commit = Text(snapshot.Commit)
	snapshot.TaskDir = Text(snapshot.TaskDir)
	snapshot.TaskOutputDir = Text(snapshot.TaskOutputDir)
	snapshot.TestsAnalysis = Text(snapshot.TestsAnalysis)
	snapshot.QwenResult = Text(snapshot.QwenResult)
	snapshot.OpusResult = Text(snapshot.OpusResult)
	snapshot.SimilarityHistoryDirs = StringSlice(snapshot.SimilarityHistoryDirs)
	snapshot.SimilarityTB3Dirs = StringSlice(snapshot.SimilarityTB3Dirs)
	snapshot.HarborAgent = Text(snapshot.HarborAgent)
	snapshot.HarborAgentEnvKeys = StringSlice(snapshot.HarborAgentEnvKeys)
	snapshot.QwenModel = Text(snapshot.QwenModel)
	snapshot.OpusModel = Text(snapshot.OpusModel)
	snapshot.QwenHarborBaseURL = Text(snapshot.QwenHarborBaseURL)
	snapshot.OpusHarborBaseURL = Text(snapshot.OpusHarborBaseURL)
	snapshot.OutputDir = Text(snapshot.OutputDir)
	snapshot.TaskName = Text(snapshot.TaskName)
	snapshot.CodeLang = Text(snapshot.CodeLang)
	snapshot.TaskType = Text(snapshot.TaskType)
	snapshot.Application = Text(snapshot.Application)
	snapshot.AHT = Text(snapshot.AHT)
	snapshot.Description = Text(snapshot.Description)
	snapshot.QwenScreenshot = Text(snapshot.QwenScreenshot)
	snapshot.OpusScreenshot = Text(snapshot.OpusScreenshot)
	snapshot.Model = Text(snapshot.Model)
	snapshot.Reasoning = Text(snapshot.Reasoning)
	snapshot.CodexPath = Text(snapshot.CodexPath)
	snapshot.RepairGuidance = Text(snapshot.RepairGuidance)
	snapshot.RepairSource = Text(snapshot.RepairSource)
	snapshot.SensitiveFieldsOmitted = StringSlice(snapshot.SensitiveFieldsOmitted)
	snapshot.UnsupportedFieldsOmitted = StringSlice(snapshot.UnsupportedFieldsOmitted)
	return snapshot
}
