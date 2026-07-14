package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestTaskHubFrozenExecutionFromRunProjectsBoundCatalogReceipt(t *testing.T) {
	run, expectedCatalog := taskHubFrozenExecutionRunFixture(t)
	fact := taskHubFrozenExecutionFromRun(run)
	if fact.State != TaskHubFrozenExecutionBound {
		t.Fatalf("frozen execution state = %q, want bound", fact.State)
	}
	if fact.TemplateID != run.WorkflowTemplateID || fact.TemplateVersion != run.WorkflowTemplateVersion || fact.ProfileFingerprint != run.ResolvedProfileHash || fact.DefinitionFingerprint != run.DefinitionHash {
		t.Fatalf("frozen execution identity = %+v, run = %+v", fact, run)
	}
	if fact.ContinuationPlanTTL != workflowadapter.RequiredContinuationPlanTTL || fact.ControlGracePeriod != 45*time.Second || fact.InputBundleID == "" || fact.ExecutionSpecFingerprint == "" {
		t.Fatalf("frozen execution policy/input projection = %+v", fact)
	}
	if fact.DeploymentCatalog.State != TaskHubDeploymentCatalogBound || fact.DeploymentCatalog.CatalogID != expectedCatalog.CatalogID || fact.DeploymentCatalog.CatalogVersion != expectedCatalog.CatalogVersion || fact.DeploymentCatalog.CatalogFingerprint != string(expectedCatalog.CatalogFingerprint) {
		t.Fatalf("catalog receipt projection = %+v, want %+v", fact.DeploymentCatalog, expectedCatalog)
	}
}

func TestTaskHubFrozenExecutionFromRunRejectsDuplicateAndDriftingManifest(t *testing.T) {
	run, _ := taskHubFrozenExecutionRunFixture(t)
	run.RunManifestJSON = strings.Replace(run.RunManifestJSON, `"run_id":"run-frozen-1"`, `"run_id":"run-frozen-1","run_id":"another-run"`, 1)
	if fact := taskHubFrozenExecutionFromRun(run); fact.State != TaskHubFrozenExecutionInvalid {
		t.Fatalf("duplicate-key manifest state = %q, want invalid", fact.State)
	}

	run, _ = taskHubFrozenExecutionRunFixture(t)
	run.RunManifestJSON = strings.Replace(run.RunManifestJSON, `"definition_fingerprint":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"`, `"definition_fingerprint":"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"`, 1)
	if fact := taskHubFrozenExecutionFromRun(run); fact.State != TaskHubFrozenExecutionInvalid {
		t.Fatalf("definition-drift manifest state = %q, want invalid", fact.State)
	}
}

func TestTaskHubFrozenExecutionFromRunRetainsInvalidCatalogAsVisibleNonExecutableState(t *testing.T) {
	run, _ := taskHubFrozenExecutionRunFixture(t)
	run.RunManifestJSON = strings.Replace(run.RunManifestJSON, `"catalog_version":"2026.07"`, `"catalog_version":""`, 1)
	fact := taskHubFrozenExecutionFromRun(run)
	if fact.State != TaskHubFrozenExecutionBound {
		t.Fatalf("invalid receipt must not discard a separately bound manifest: state=%q", fact.State)
	}
	if fact.DeploymentCatalog.State != TaskHubDeploymentCatalogInvalid || fact.DeploymentCatalog.CatalogID != "" {
		t.Fatalf("invalid catalog fact = %+v, want non-partial invalid state", fact.DeploymentCatalog)
	}
}

func taskHubFrozenExecutionRunFixture(t *testing.T) (store.WorkflowRun, stageprovider.DeploymentOperationCatalogReceipt) {
	t.Helper()
	template := workflowadapter.StandardTemplateReference()
	const (
		templateFingerprint   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		profileFingerprint    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		definitionFingerprint = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		manifestFingerprint   = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
		planFingerprint       = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		specFingerprint       = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		catalogFingerprint    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	)
	receipt := stageprovider.DeploymentOperationCatalogReceipt{
		Format:               stageprovider.DeploymentOperationCatalogReceiptFormat,
		Version:              stageprovider.DeploymentOperationCatalogReceiptVersion,
		CatalogFormat:        stageprovider.DeploymentOperationCatalogFormat,
		CatalogSchemaVersion: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID:            "codeedge-phase1-production",
		CatalogVersion:       "2026.07",
		Template:             template,
		CatalogFingerprint:   workflowkit.Fingerprint(catalogFingerprint),
	}
	catalogRaw, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical catalog receipt: %v", err)
	}
	manifest := taskHubRunManifestProjection{
		Format:     taskHubRunManifestFormat,
		RunID:      "run-frozen-1",
		TaskID:     "task-frozen-1",
		RevisionID: "revision-frozen-1",
		InitialExecutionPlan: taskHubExecutionPlanProjection{
			Fingerprint: planFingerprint,
		},
		Inputs: &taskHubRunManifestInputsProjection{
			Format:                   taskHubRunManifestInputsFormat,
			BundleID:                 "019f5e99-a5eb-7a18-8832-7a4498af9d6b",
			ProfileFingerprint:       profileFingerprint,
			ExecutionSpecFingerprint: specFingerprint,
		},
		DeploymentCatalog: catalogRaw,
	}
	manifest.Resolved.Template.ID = template.ID
	manifest.Resolved.Template.Version = template.Version
	manifest.Resolved.TemplateID = template.ID
	manifest.Resolved.TemplateVersion = template.Version
	manifest.Resolved.ExecutionProfileID = "codeedge-phase1-local"
	manifest.Resolved.ExecutionProfileVersion = "2026.07"
	manifest.Resolved.ContinuationPlanTTL = workflowadapter.RequiredContinuationPlanTTL
	manifest.Resolved.ControlGracePeriod = 45 * time.Second
	manifest.Resolved.TemplateFingerprint = templateFingerprint
	manifest.Resolved.ExecutionProfileFingerprint = profileFingerprint
	manifest.Resolved.DefinitionFingerprint = definitionFingerprint
	manifest.Resolved.ManifestFingerprint = manifestFingerprint
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode frozen Run manifest fixture: %v", err)
	}
	return store.WorkflowRun{
		ID:                      manifest.RunID,
		TaskID:                  manifest.TaskID,
		RevisionID:              manifest.RevisionID,
		WorkflowTemplateID:      template.ID,
		WorkflowTemplateVersion: template.Version,
		ResolvedProfileHash:     profileFingerprint,
		DefinitionHash:          definitionFingerprint,
		RunManifestJSON:         string(raw),
	}, receipt
}
