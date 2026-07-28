package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
)

func TestUpgradeKnownLegacyConsolidatedV2Schema(t *testing.T) {
	currentTrigger, err := currentAuthoringPhase1HandoffTriggerSQL()
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name                 string
		trigger              string
		fingerprint          string
		expectsLegacyTrigger bool
	}{
		{name: "1.2 only", trigger: legacyV12AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV12ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "1.2 and 1.3", trigger: legacyV13AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV13ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "through 1.4", trigger: legacyV14AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV14ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "through 1.5", trigger: legacyV15AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV15ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "through 1.6", trigger: legacyV16AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV16ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "through 1.7", trigger: legacyV17AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV17ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
		{name: "through 1.8", trigger: legacyV18AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV18ConsolidatedV2SchemaContractFingerprint, expectsLegacyTrigger: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := tempDB(t)
			root := store.rootDir
			removeAgentTurnTranscriptSchemaForLegacyTest(t, store)
			if testCase.expectsLegacyTrigger && testCase.trigger == currentTrigger {
				t.Fatal("legacy trigger fixture was not changed")
			}
			if _, err := store.db.Exec(`DROP INDEX ` + codeEdgeEvaluatorParentIndexName); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`DROP TRIGGER ` + authoringPhase1HandoffTriggerName); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(testCase.trigger); err != nil {
				t.Fatal(err)
			}
			legacyFingerprint, err := sqliteSchemaContract(store.db)
			if err != nil {
				t.Fatal(err)
			}
			if legacyFingerprint != testCase.fingerprint {
				t.Fatalf("legacy fixture fingerprint = %s, want %s", legacyFingerprint, testCase.fingerprint)
			}
			if _, err := store.db.Exec(`UPDATE store_metadata SET value = ? WHERE key = ?`, legacyFingerprint, baselineV2SchemaContractMetadataKey); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			upgraded, err := Open(root)
			if err != nil {
				t.Fatalf("open legacy V2 store for atomic upgrade: %v", err)
			}
			defer upgraded.Close()
			if err := upgraded.validateConsolidatedV2Baseline(); err != nil {
				t.Fatalf("upgraded store failed current schema admission: %v", err)
			}
			expected, err := consolidatedV2SchemaContract()
			if err != nil {
				t.Fatal(err)
			}
			actual, err := sqliteSchemaContract(upgraded.db)
			if err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("upgraded schema fingerprint = %s, want %s", actual, expected)
			}
			var recorded string
			if err := upgraded.db.QueryRowContext(context.Background(), `SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&recorded); err != nil {
				t.Fatal(err)
			}
			if recorded != expected {
				t.Fatalf("upgraded schema marker = %s, want %s", recorded, expected)
			}
		})
	}
}

func TestUpgradeLegacySchemaRefusesUnfinishedWorkflowRunWithoutMutation(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	root := store.rootDir
	_, _, activeRun := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	if activeRun.Status != WorkflowRunQueued {
		t.Fatalf("fixture Run status = %s, want %s", activeRun.Status, WorkflowRunQueued)
	}
	prepareLegacyV18SchemaUpgradeFixture(t, store)
	legacyContract, err := sqliteSchemaContract(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if legacyContract != legacyV18ConsolidatedV2SchemaContractFingerprint {
		t.Fatalf("legacy V18 contract = %s, want %s", legacyContract, legacyV18ConsolidatedV2SchemaContractFingerprint)
	}
	if _, err := store.db.Exec(`UPDATE store_metadata SET value = ? WHERE key = ?`, legacyContract, baselineV2SchemaContractMetadataKey); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(root); !errors.Is(err, ErrActiveRunSchemaUpgrade) || !strings.Contains(err.Error(), activeRun.ID) || !strings.Contains(err.Error(), "finish it with the deployment package") {
		t.Fatalf("active legacy upgrade error = %v, want %v naming %s", err, ErrActiveRunSchemaUpgrade, activeRun.ID)
	}
	uri, err := sqliteFileURI(filepath.Join(root, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", uri+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var recordedContract string
	if err := database.QueryRowContext(ctx, `SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&recordedContract); err != nil {
		t.Fatal(err)
	}
	if recordedContract != legacyContract {
		t.Fatalf("blocked legacy upgrade rewrote schema marker = %s, want %s", recordedContract, legacyContract)
	}
	var indexCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, codeEdgeEvaluatorParentIndexName).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 0 {
		t.Fatalf("blocked legacy upgrade installed evaluator index")
	}
}

func TestCurrentSchemaAllowsUnfinishedWorkflowRun(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	root := store.rootDir
	_, _, activeRun := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("open current schema with unfinished Run %s: %v", activeRun.ID, err)
	}
	defer reopened.Close()
}

func TestLegacySchemaUpgradeTransactionRefusesUnfinishedWorkflowRun(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	_, _, activeRun := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	prepareLegacyV18SchemaUpgradeFixture(t, store)
	legacyContract, err := sqliteSchemaContract(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if legacyContract != legacyV18ConsolidatedV2SchemaContractFingerprint {
		t.Fatalf("legacy V18 contract = %s, want %s", legacyContract, legacyV18ConsolidatedV2SchemaContractFingerprint)
	}
	if _, err := store.db.Exec(`UPDATE store_metadata SET value = ? WHERE key = ?`, legacyContract, baselineV2SchemaContractMetadataKey); err != nil {
		t.Fatal(err)
	}

	if err := store.upgradeLegacyConsolidatedV2Schema(); !errors.Is(err, ErrActiveRunSchemaUpgrade) || !strings.Contains(err.Error(), activeRun.ID) {
		t.Fatalf("transactional active legacy upgrade error = %v, want %v naming %s", err, ErrActiveRunSchemaUpgrade, activeRun.ID)
	}
	actualContract, err := sqliteSchemaContract(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if actualContract != legacyContract {
		t.Fatalf("transactional blocked upgrade rewrote schema = %s, want %s", actualContract, legacyContract)
	}
}

func TestCodeEdgeEvaluatorChildParentIsDurablyUnique(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	task, revision, parent := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	_ = schemaUpgradeEvaluatorChildFixture(t, ctx, store, task, revision, parent, "first")
	if _, err := schemaUpgradeEvaluatorChild(ctx, store, task, revision, parent, "second"); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("second evaluator child error = %v, want %v", err, ErrIdentityCollision)
	}
}

func TestCodeEdgeEvaluatorChildParentUniquenessSurvivesConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	task, revision, parent := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	const writers = 6
	start := make(chan struct{})
	results := make(chan error, writers)
	var workers sync.WaitGroup
	for index := 0; index < writers; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			<-start
			_, err := schemaUpgradeEvaluatorChild(ctx, store, task, revision, parent, "concurrent-"+string(rune('a'+index)))
			results <- err
		}(index)
	}
	close(start)
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent evaluator child creations succeeded %d times, want one", successes)
	}
	runs, err := store.ListWorkflowRunsForTask(ctx, task.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("concurrent evaluator child creations persisted runs = %+v, %v", runs, err)
	}
	if _, err := schemaUpgradeEvaluatorChild(ctx, store, task, revision, parent, "after-concurrency"); !errors.Is(err, ErrIdentityCollision) {
		t.Fatalf("post-concurrency evaluator child error = %v, want %v", err, ErrIdentityCollision)
	}
}

func TestUpgradeLegacyV18SchemaRejectsDuplicateEvaluatorChildrenWithoutPartialUpgrade(t *testing.T) {
	ctx := context.Background()
	store := tempDB(t)
	root := store.rootDir
	task, revision, parent := schemaUpgradeEvaluatorParentFixture(t, ctx, store)
	_ = schemaUpgradeEvaluatorChildFixture(t, ctx, store, task, revision, parent, "first")
	prepareLegacyV18SchemaUpgradeFixture(t, store)
	if _, err := schemaUpgradeEvaluatorChild(ctx, store, task, revision, parent, "legacy-duplicate"); err != nil {
		t.Fatalf("create legacy duplicate evaluator child: %v", err)
	}
	legacyContract, err := sqliteSchemaContract(store.db)
	if err != nil {
		t.Fatal(err)
	}
	if legacyContract != legacyV18ConsolidatedV2SchemaContractFingerprint {
		t.Fatalf("legacy V18 contract = %s, want %s", legacyContract, legacyV18ConsolidatedV2SchemaContractFingerprint)
	}
	if _, err := store.db.Exec(`UPDATE store_metadata SET value = ? WHERE key = ?`, legacyContract, baselineV2SchemaContractMetadataKey); err != nil {
		t.Fatal(err)
	}
	// This fixture verifies duplicate-child DDL failure independently from the
	// active-Run protection. Mark all rows terminal so schema admission reaches
	// the duplicate-index check.
	if _, err := store.db.Exec(`UPDATE workflow_runs SET status = ?, finished_at = CURRENT_TIMESTAMP`, WorkflowRunSucceeded); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "multiple CodeEdge evaluator child Runs") {
		t.Fatalf("duplicate legacy evaluator children upgrade error = %v", err)
	}
	uri, err := sqliteFileURI(filepath.Join(root, dbFileName))
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", uri+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var recordedContract string
	if err := database.QueryRowContext(ctx, `SELECT value FROM store_metadata WHERE key = ?`, baselineV2SchemaContractMetadataKey).Scan(&recordedContract); err != nil {
		t.Fatal(err)
	}
	if recordedContract != legacyContract {
		t.Fatalf("failed legacy upgrade rewrote schema marker = %s, want %s", recordedContract, legacyContract)
	}
	var indexCount int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, codeEdgeEvaluatorParentIndexName).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 0 {
		t.Fatalf("failed legacy upgrade installed evaluator index despite duplicate children")
	}
}

func schemaUpgradeEvaluatorParentFixture(t *testing.T, ctx context.Context, store *Store) (TaskV2, TaskRevision, WorkflowRun) {
	t.Helper()
	task, revision := createValidatedTaskAndRevision(t, store)
	parent, err := store.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: workflowadapter.CodeEdgePhase1WorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgePhase1WorkflowTemplateVersion,
		ResolvedProfileHash: "schema-upgrade-evaluator-parent-profile", DefinitionHash: "schema-upgrade-evaluator-parent-definition", RunManifestJSON: `{}`,
		Trigger: "schema-upgrade-evaluator-parent", Actor: "tester", Reason: "create evaluator parent fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	return task, revision, parent
}

func schemaUpgradeEvaluatorChildFixture(t *testing.T, ctx context.Context, store *Store, task TaskV2, revision TaskRevision, parent WorkflowRun, suffix string) WorkflowRun {
	t.Helper()
	child, err := schemaUpgradeEvaluatorChild(ctx, store, task, revision, parent, suffix)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func schemaUpgradeEvaluatorChild(ctx context.Context, store *Store, task TaskV2, revision TaskRevision, parent WorkflowRun, suffix string) (WorkflowRun, error) {
	return store.CreateWorkflowRun(ctx, CreateWorkflowRunRequest{
		TaskID: task.ID, RevisionID: revision.ID,
		WorkflowTemplateID: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, WorkflowTemplateVersion: workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion,
		ResolvedProfileHash: "schema-upgrade-evaluator-child-profile-" + suffix,
		DefinitionHash:      "schema-upgrade-evaluator-child-definition-" + suffix,
		RunManifestJSON:     `{}`,
		ParentRunID:         parent.ID,
		Trigger:             "schema-upgrade-evaluator-child",
		Actor:               "tester",
		Reason:              "create evaluator child fixture",
	})
}

func legacyV12AuthoringPhase1HandoffTrigger(current string) string {
	legacy := strings.Replace(legacyV16AuthoringPhase1HandoffTrigger(current),
		"      AND (\n          (run.workflow_template_version = '1.2.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v1')\n          OR\n          (run.workflow_template_version = '1.3.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.4.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.5.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n      )",
		"      AND run.workflow_template_version = '1.2.0'",
		1,
	)
	return strings.Replace(legacy,
		"      AND artifact.artifact_key = 'authoring_task_handoff'\n",
		"      AND artifact.artifact_key = 'authoring_task_handoff'\n      AND artifact.schema_version = 'harbor.authoring-task-handoff.v1'\n",
		1,
	)
}

func legacyV13AuthoringPhase1HandoffTrigger(current string) string {
	legacy := strings.Replace(legacyV16AuthoringPhase1HandoffTrigger(current),
		"          OR\n          (run.workflow_template_version = '1.4.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
	legacy = strings.Replace(legacy,
		"          OR\n          (run.workflow_template_version = '1.5.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
	return strings.Replace(legacy,
		"          OR\n          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func legacyV14AuthoringPhase1HandoffTrigger(current string) string {
	legacy := strings.Replace(legacyV16AuthoringPhase1HandoffTrigger(current),
		"          OR\n          (run.workflow_template_version = '1.5.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
	return strings.Replace(legacy,
		"          OR\n          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func legacyV15AuthoringPhase1HandoffTrigger(current string) string {
	return strings.Replace(legacyV16AuthoringPhase1HandoffTrigger(current),
		"          OR\n          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func legacyV16AuthoringPhase1HandoffTrigger(current string) string {
	return strings.Replace(legacyV17AuthoringPhase1HandoffTrigger(current),
		"          OR\n          (run.workflow_template_version = '1.7.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func legacyV17AuthoringPhase1HandoffTrigger(current string) string {
	return strings.Replace(legacyV18AuthoringPhase1HandoffTrigger(current),
		"          OR\n          (run.workflow_template_version = '1.8.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func legacyV18AuthoringPhase1HandoffTrigger(current string) string {
	return strings.Replace(current,
		"      AND run.workflow_template_version = '2.0.0'\n      AND artifact.schema_version = 'harbor.authoring-task-handoff.v2'",
		"      AND (\n          (run.workflow_template_version = '1.2.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v1')\n          OR\n          (run.workflow_template_version = '1.3.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.4.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.5.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.6.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.7.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n          OR\n          (run.workflow_template_version = '1.8.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n      )",
		1,
	)
}

func prepareLegacyV18SchemaUpgradeFixture(t *testing.T, store *Store) {
	t.Helper()
	removeAgentTurnTranscriptSchemaForLegacyTest(t, store)
	currentTrigger, err := currentAuthoringPhase1HandoffTriggerSQL()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX ` + codeEdgeEvaluatorParentIndexName); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER ` + authoringPhase1HandoffTriggerName); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(legacyV18AuthoringPhase1HandoffTrigger(currentTrigger)); err != nil {
		t.Fatal(err)
	}
}

// removeAgentTurnTranscriptSchemaForLegacyTest makes an exact pre-transcript
// V18 fixture. The published V18 contract predates all of these tables,
// indexes, and triggers; retaining any one would make its fingerprint an
// invented schema which Open must reject rather than upgrade.
func removeAgentTurnTranscriptSchemaForLegacyTest(t *testing.T, store *Store) {
	t.Helper()
	for _, trigger := range []string{
		"agent_turn_transcripts_no_delete",
		"agent_turn_transcripts_expiry_only_update",
		"agent_turn_transcript_submissions_no_delete",
		"agent_turn_transcript_submissions_expiry_only_update",
		"agent_turn_transcript_legal_holds_no_delete",
		"agent_turn_transcript_legal_holds_release_only_update",
		"entity_id_registry_agent_turn_transcripts_id_immutable",
		"entity_id_registry_agent_turn_transcripts_insert",
		"entity_id_registry_agent_turn_transcript_submissions_id_immutable",
		"entity_id_registry_agent_turn_transcript_submissions_insert",
		"entity_id_registry_agent_turn_transcript_legal_holds_id_immutable",
		"entity_id_registry_agent_turn_transcript_legal_holds_insert",
	} {
		if _, err := store.db.Exec(`DROP TRIGGER ` + trigger); err != nil {
			t.Fatal(err)
		}
	}
	for _, index := range []string{
		"idx_agent_turn_transcripts_expiry",
		"idx_agent_turn_transcripts_node",
		"idx_agent_turn_transcript_submissions_transcript",
		"idx_agent_turn_transcript_legal_holds_active",
	} {
		if _, err := store.db.Exec(`DROP INDEX ` + index); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{
		"agent_turn_transcript_submissions",
		"agent_turn_transcript_legal_holds",
		"agent_turn_transcripts",
	} {
		if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
}
