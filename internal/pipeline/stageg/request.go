package stageg

import (
	"context"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/scanner"
)

type ArtifactWriter interface {
	RequiredJSON(path string, value any) error
	RequiredText(path, content string) error
	BestEffortJSON(path string, value any) ArtifactWarning
	BestEffortText(path, content string) ArtifactWarning
	BestEffortAppend(path, content string) ArtifactWarning
	Path(path string) string
	RelativePath(path string) string
}

type PlannerFunc func(ctx context.Context, promptTemplate, profile, contextText string, candidates []BrowserURLCandidate, observations []browserpkg.Observation, blocked []BlockedBrowserAction, round int, timeout time.Duration) (string, []ArtifactWarning, error)

type ActionRunnerFunc func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error)

type ProgressFunc func(round int, action string, ok bool)

type Request struct {
	Run                model.RunRecord
	Project            scanner.Project
	Candidates         []BrowserURLCandidate
	HasCleanupTarget   bool
	Writer             ArtifactWriter
	Timeout            time.Duration
	PromptProfilesDir  string
	PlannerTurnTimeout time.Duration
	Planner            PlannerFunc
	ActionRunner       ActionRunnerFunc
	Progress           ProgressFunc
}

type StageContext = Request

type Runner struct {
	request Request
}

func Run(ctx context.Context, request Request) model.StageRecord {
	return Runner{request: request}.stageG(ctx, request)
}
