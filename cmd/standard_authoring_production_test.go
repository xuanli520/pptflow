package cmd

import (
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestLinkedStandardAuthoringProductionBuildBindingFailsClosedWithoutLinkerValues(t *testing.T) {
	if _, err := linkedStandardAuthoringProductionBuildBinding(); err == nil {
		t.Fatal("unlinked Standard authoring build unexpectedly exposed a production capability")
	}
}

func TestStandardAuthoringLockedGitSelectsOnlyTheClosedRepoPrepareCommand(t *testing.T) {
	locked := stageprovider.LocalExecutableLock{
		CommandID:     stageprovider.StandardAuthoringGitSnapshotCommandID,
		AbsolutePath:  "/opt/harbor/git",
		Version:       "2.47.3",
		ContentSHA256: workflowkit.SHA256Fingerprint([]byte("locked-git")),
	}
	lock := stageprovider.DeploymentOperationCatalogLock{Operations: []stageprovider.DeploymentOperationCatalogLockRecord{
		{
			Stage: stageprovider.DeploymentStageContract{Key: workflowkit.StageKey(workflowadapter.RepoPrepare)},
			Operation: workflowadapter.StageOperationBinding{
				Payload: workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{}},
			},
			LocalExecutable: &locked,
		},
	}}
	selected, err := standardAuthoringLockedGit(lock)
	if err != nil {
		t.Fatalf("select closed Standard Git lock: %v", err)
	}
	if selected != locked {
		t.Fatalf("selected lock = %+v, want %+v", selected, locked)
	}

	for name, mutate := range map[string]func(*stageprovider.DeploymentOperationCatalogLock){
		"wrong command": func(candidate *stageprovider.DeploymentOperationCatalogLock) {
			candidate.Operations[0].Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "other-command", Arguments: []string{}}
		},
		"arguments": func(candidate *stageprovider.DeploymentOperationCatalogLock) {
			candidate.Operations[0].Operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{"--unsafe"}}
		},
		"missing executable": func(candidate *stageprovider.DeploymentOperationCatalogLock) {
			candidate.Operations[0].LocalExecutable = nil
		},
		"conflicting executable": func(candidate *stageprovider.DeploymentOperationCatalogLock) {
			other := locked
			other.AbsolutePath = "/opt/harbor/other-git"
			candidate.Operations = append(candidate.Operations, stageprovider.DeploymentOperationCatalogLockRecord{
				Stage:           stageprovider.DeploymentStageContract{Key: workflowkit.StageKey(workflowadapter.RepoPrepare)},
				Operation:       workflowadapter.StageOperationBinding{Payload: workflowadapter.LocalCommandOperationPayload{CommandID: stageprovider.StandardAuthoringGitSnapshotCommandID, Arguments: []string{}}},
				LocalExecutable: &other,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := lock.Clone()
			if candidate.Operations[0].LocalExecutable != nil {
				copy := *candidate.Operations[0].LocalExecutable
				candidate.Operations[0].LocalExecutable = &copy
			}
			mutate(&candidate)
			if _, err := standardAuthoringLockedGit(candidate); err == nil {
				t.Fatal("invalid Standard authoring Git selection was accepted")
			}
		})
	}
}
