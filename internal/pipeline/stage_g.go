package pipeline

import (
	"context"
	"fmt"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
	"github.com/xuanli520/p2r_tui/internal/pipeline/stageg"
)

func (r Runner) stageG(ctx context.Context, sc StageContext) model.StageRecord {
	nodePath := sc.Preflight.NodePath()
	return stageg.Run(ctx, stageg.Request{
		Run:                sc.Run,
		Project:            sc.Project,
		Candidates:         browserURLCandidates(sc.Runtime),
		HasCleanupTarget:   sc.Runtime.HasCleanupTarget(),
		Writer:             artifactWriterForStageContext(sc),
		Timeout:            stageTimeoutForStageContext(sc, r, string(model.StageG), 600),
		PromptProfilesDir:  r.cfg.Codex.PromptProfilesDir,
		PlannerTurnTimeout: stageg.PlannerTurnTimeout(r.cfg.Pipeline.StageG),
		Planner: func(ctx context.Context, promptTemplate, profile, contextText string, candidates []stageg.BrowserURLCandidate, observations []browserpkg.Observation, blocked []stageg.BlockedBrowserAction, round int, timeout time.Duration) (string, []stageg.ArtifactWarning, error) {
			if r.stageGBrowserPlan != nil {
				return r.stageGBrowserPlan(ctx, sc, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
			}
			return r.nextBrowserAction(ctx, sc, promptTemplate, profile, contextText, candidates, observations, blocked, round, timeout)
		},
		ActionRunner: func(ctx context.Context, action browserpkg.Action, policy browserpkg.Policy, timeout time.Duration) (browserpkg.Observation, error) {
			if r.stageGBrowserAction != nil {
				return r.stageGBrowserAction(ctx, action, policy, timeout)
			}
			runner := browserpkg.NewPlaywrightWrapper(r.exec, nodePath, policy)
			return runner.Run(ctx, action, timeout)
		},
		Progress: func(round int, action string, ok bool) {
			appendStreamProgress(sc.Run.RunID, string(model.StageG), fmt.Sprintf("G action %d: %s -> ok=%t", round, action, ok), "p2r", false, sc.Progress)
		},
	})
}
