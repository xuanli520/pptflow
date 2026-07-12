package gen

import (
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	prompttemplates "github.com/purplevoid/harbor-factory/internal/templates"
)

type repoAnalyzePromptData struct {
	RepoURL   string
	CommitSHA string
	TreeHash  string
}

type taskDesignPromptData struct {
	RepoAnalysisJSON string
}

type taskFilesPromptData struct {
	RepoAnalysisJSON string
	TaskProposalJSON string
}

func repoAnalyzePrompt(prepared domain.RepoPrepared) (string, error) {
	return renderPrompt("phase1/repo_analyze", repoAnalyzePromptData{
		RepoURL:   prepared.RepoURL,
		CommitSHA: prepared.ResolvedCommit,
		TreeHash:  prepared.TreeHash,
	})
}

func taskDesignPrompt(repoAnalysisJSON string) (string, error) {
	return renderPrompt("phase1/task_design", taskDesignPromptData{RepoAnalysisJSON: repoAnalysisJSON})
}

func taskFilesPrompt(repoAnalysisJSON, proposalJSON string) (string, error) {
	return renderPrompt("phase1/task_files", taskFilesPromptData{
		RepoAnalysisJSON: repoAnalysisJSON,
		TaskProposalJSON: proposalJSON,
	})
}

func runtimeSelfCheckPrompt() (string, error) {
	return renderPrompt("phase2/runtime_self_check", struct{}{})
}

func renderPrompt(name string, data any) (string, error) {
	engine, err := prompttemplates.Default()
	if err != nil {
		return "", fmt.Errorf("load prompt templates: %w", err)
	}
	prompt, err := engine.Render(name, data)
	if err != nil {
		return "", err
	}
	return prompt, nil
}
