package store

import (
	"context"
	"strings"
	"testing"
)

func TestUpgradeKnownLegacyConsolidatedV2Schema(t *testing.T) {
	currentTrigger, err := currentAuthoringPhase1HandoffTriggerSQL()
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name        string
		trigger     string
		fingerprint string
	}{
		{name: "1.2 only", trigger: legacyV12AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV12ConsolidatedV2SchemaContractFingerprint},
		{name: "1.2 and 1.3", trigger: legacyV13AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV13ConsolidatedV2SchemaContractFingerprint},
		{name: "through 1.4", trigger: legacyV14AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV14ConsolidatedV2SchemaContractFingerprint},
		{name: "through 1.5", trigger: legacyV15AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV15ConsolidatedV2SchemaContractFingerprint},
		{name: "through 1.6", trigger: legacyV16AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV16ConsolidatedV2SchemaContractFingerprint},
		{name: "through 1.7", trigger: legacyV17AuthoringPhase1HandoffTrigger(currentTrigger), fingerprint: legacyV17ConsolidatedV2SchemaContractFingerprint},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			store := tempDB(t)
			root := store.rootDir
			if testCase.trigger == currentTrigger {
				t.Fatal("legacy trigger fixture was not changed")
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
	return strings.Replace(current,
		"          OR\n          (run.workflow_template_version = '1.8.0' AND artifact.schema_version = 'harbor.authoring-task-handoff.v2')\n",
		"",
		1,
	)
}

func TestAuthoringPhase1HandoffLineageConstraintIsDetected(t *testing.T) {
	if !isAuthoringPhase1HandoffLineageConstraint(assertionError("constraint failed: authoring Phase-1 handoff does not match persisted materialization lineage (1811)")) {
		t.Fatal("SQLite authoring handoff lineage trigger was not recognized")
	}
	if isAuthoringPhase1HandoffLineageConstraint(assertionError("database is temporarily unavailable")) {
		t.Fatal("generic storage failure was classified as a lineage constraint")
	}
}

type assertionError string

func (err assertionError) Error() string { return string(err) }
