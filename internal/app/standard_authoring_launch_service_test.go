package app

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

var standardAuthoringLaunchTestCoordinate = StandardAuthoringSourceCoordinate{
	RepositoryURL: "https://github.com/example/fixture-repository.git",
	CommitSHA:     "0123456789abcdef0123456789abcdef01234567",
}

func TestStandardAuthoringLaunchCapturesSourceCreatesRevisionFreeTaskAndQueuesRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "create immutable source task"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "fixture-authoring", Title: "Fixture authoring", MetadataJSON: `{"difficulty":"hard"}`,
	}
	receipt, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatalf("start Standard authoring: %v", err)
	}
	if receipt.Action != standardAuthoringLaunchAction || receipt.AuthoringSourceID == "" || receipt.AuthoringSessionID == "" || receipt.TaskID == "" || receipt.RunID == "" {
		t.Fatalf("launch receipt is incomplete: %+v", receipt)
	}
	if capturer.calls != 1 {
		t.Fatalf("source capturer calls = %d, want 1", capturer.calls)
	}

	source, err := database.GetAuthoringSource(ctx, receipt.AuthoringSourceID)
	if err != nil || source == nil {
		t.Fatalf("load AuthoringSource: source=%+v err=%v", source, err)
	}
	if source.RepositoryURL != command.RepositoryURL || source.CommitSHA != command.CommitSHA || source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != StandardAuthoringSourceSnapshotSchemaVersion {
		t.Fatalf("frozen source = %+v", source)
	}
	object, err := services.core.objects.ReadAll(ctx, workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(source.SnapshotContentDigest), SizeBytes: int64(len(capturer.snapshot.Content))})
	if err != nil || !bytes.Equal(object, capturer.snapshot.Content) {
		t.Fatalf("source object = %d bytes err=%v", len(object), err)
	}

	task, err := database.GetTaskV2(ctx, receipt.TaskID)
	if err != nil || task == nil {
		t.Fatalf("load draft Task: task=%+v err=%v", task, err)
	}
	if task.LifecycleState != store.TaskLifecycleDraft || task.CurrentRevisionID != "" || task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA {
		t.Fatalf("authoring task must be a source-bound revision-free draft: %+v", task)
	}
	session, err := database.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
	if err != nil || session == nil || session.SourceID != source.ID || session.TargetTaskID != task.ID {
		t.Fatalf("authoring session = %+v err=%v", session, err)
	}
	run, err := database.GetWorkflowRun(ctx, receipt.RunID)
	if err != nil || run == nil {
		t.Fatalf("load authoring Run: run=%+v err=%v", run, err)
	}
	if run.SubjectKind != store.WorkflowRunSubjectAuthoringSession || run.TaskID != "" || run.RevisionID != "" || run.AuthoringSessionID != session.ID || run.SubjectID != source.ID || run.SubjectRevisionID != session.ID || run.SubjectDigest != source.SnapshotContentDigest {
		t.Fatalf("authoring Run subject = %+v", run)
	}
	if _, _, err := services.core.verifyRunManagedExecutionInputs(ctx, *run); err != nil {
		t.Fatalf("verify frozen authoring execution inputs: %v", err)
	}

	replayed, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatalf("replay Standard authoring: %v", err)
	}
	if replayed != receipt || capturer.calls != 1 {
		t.Fatalf("replay = %+v; first=%+v; capturer calls=%d", replayed, receipt, capturer.calls)
	}
}

func TestStandardAuthoringLaunchReusesMatchingImmutableSourceForDistinctTasks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	firstKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	first, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: firstKey, Actor: "author", Reason: "create first task from frozen source"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "first-source-reuse-task",
		Title:                        "First source reuse task",
		MetadataJSON:                 `{"ordinal":1}`,
	})
	if err != nil {
		t.Fatalf("start first Standard authoring task: %v", err)
	}
	secondKey, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	second, err := services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: secondKey, Actor: "author", Reason: "create second task from same frozen source"},
		RepositoryURL:                "https://GitHub.com/example/fixture-repository.git/",
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "second-source-reuse-task",
		Title:                        "Second source reuse task",
		MetadataJSON:                 `{"ordinal":2}`,
	})
	if err != nil {
		t.Fatalf("start second Standard authoring task: %v", err)
	}
	if second.AuthoringSourceID != first.AuthoringSourceID {
		t.Fatalf("matching immutable snapshots must reuse one AuthoringSource: first=%+v second=%+v", first, second)
	}
	if second.TaskID == first.TaskID || second.AuthoringSessionID == first.AuthoringSessionID || second.RunID == first.RunID {
		t.Fatalf("source reuse merged distinct authoring launches: first=%+v second=%+v", first, second)
	}
	if capturer.calls != 2 {
		t.Fatalf("source capturer calls = %d, want one capture per distinct launch", capturer.calls)
	}

	source, err := database.GetAuthoringSource(ctx, first.AuthoringSourceID)
	if err != nil || source == nil {
		t.Fatalf("load reused AuthoringSource: source=%+v err=%v", source, err)
	}
	if source.IdempotencyKey != standardAuthoringLaunchChildKey(firstKey, "source") {
		t.Fatalf("reused AuthoringSource identity changed: %+v", source)
	}
	secondOperation, err := database.GetLifecycleOperationByIdempotencyKey(ctx, secondKey)
	if err != nil || secondOperation == nil {
		t.Fatalf("load second source-reuse lifecycle operation: operation=%+v err=%v", secondOperation, err)
	}
	operationDirectory, err := services.AuthoringLaunches.openStandardAuthoringLaunchOperationDirectory(secondOperation.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer operationDirectory.Close()
	preparation, found, err := readStandardAuthoringLaunchPreparationAt(operationDirectory)
	if err != nil || !found {
		t.Fatalf("read second source-reuse preparation: found=%v err=%v", found, err)
	}
	secondIDs, err := standardAuthoringLaunchIdentities(secondKey)
	if err != nil {
		t.Fatal(err)
	}
	if preparation.RequestedSourceID != secondIDs.SourceID || preparation.RequestedSourceID == source.ID {
		t.Fatalf("preparation must retain only the launch-local requested source ID: %+v; reused source=%s", preparation, source.ID)
	}
	secondSession, err := database.GetAuthoringSession(ctx, second.AuthoringSessionID)
	if err != nil || secondSession == nil {
		t.Fatalf("load second source-reuse authoring session: session=%+v err=%v", secondSession, err)
	}
	var secondManifest standardAuthoringSessionManifest
	if err := decodeStrictJSON(secondSession.SessionManifestJSON, &secondManifest); err != nil {
		t.Fatalf("decode second source-reuse session manifest: %v", err)
	}
	if secondManifest.RequestedSourceID != secondIDs.SourceID || secondManifest.SourceID != source.ID ||
		secondManifest.RequestedSourceID == secondManifest.SourceID || secondSession.SourceID != secondManifest.SourceID {
		t.Fatalf("second source-reuse session manifest must distinguish requested and actual source IDs: manifest=%+v session=%+v", secondManifest, secondSession)
	}
	if err := verifyStandardAuthoringLaunchSourceObject(ctx, services.core.objects, *source); err != nil {
		t.Fatalf("verify reused AuthoringSource object: %v", err)
	}
	for _, receipt := range []LifecycleMutationReceipt{first, second} {
		task, err := database.GetTaskV2(ctx, receipt.TaskID)
		if err != nil || task == nil || task.SourceRepo != source.RepositoryURL || task.SourceCommit != source.CommitSHA {
			t.Fatalf("reused source task binding for receipt=%+v: task=%+v err=%v", receipt, task, err)
		}
		session, err := database.GetAuthoringSession(ctx, receipt.AuthoringSessionID)
		if err != nil || session == nil || session.SourceID != source.ID || session.TargetTaskID != task.ID {
			t.Fatalf("reused source session binding for receipt=%+v: session=%+v err=%v", receipt, session, err)
		}
	}
}

func TestStandardAuthoringLaunchPreservesSourceUUIDCollisionWhenNoMatchingSnapshotExists(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := standardAuthoringLaunchIdentities(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		ID: ids.SourceID, Slug: "occupied-source-identity", Title: "Occupied source identity",
		SourceRepo: standardAuthoringLaunchTestCoordinate.RepositoryURL, SourceCommit: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Actor: "author", Reason: "occupy derived AuthoringSource UUID",
	}); err != nil {
		t.Fatal(err)
	}
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.AuthoringLaunches.Start(ctx, StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "prove source identity is not reused"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "new-task-with-colliding-source-id",
		Title:                        "New task with colliding source ID",
		MetadataJSON:                 `{}`,
	})
	if !errors.Is(err, store.ErrIdentityCollision) {
		t.Fatalf("source UUID collision error = %v, want ErrIdentityCollision", err)
	}
	if capturer.calls != 1 {
		t.Fatalf("source capturer calls = %d, want 1", capturer.calls)
	}
	if source, err := database.GetAuthoringSource(ctx, ids.SourceID); err != nil || source != nil {
		t.Fatalf("source UUID collision created or replaced a source: source=%+v err=%v", source, err)
	}
}

func TestStandardAuthoringLaunchRejectsChangedInputForCompletedKeyWithoutRecapture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	base := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "capture source"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "fixture-source", Title: "Fixture source", MetadataJSON: `{}`,
	}
	first, err := services.AuthoringLaunches.Start(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Title = "different title"
	if _, err := services.AuthoringLaunches.Start(ctx, changed); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("changed completed launch = %v; want idempotency conflict and one capture", err)
	}
	changed = base
	changed.RepositoryURL = "https://github.com/example/other.git"
	if _, err := services.AuthoringLaunches.Start(ctx, changed); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("changed completed source = %v; want idempotency conflict and one capture", err)
	}
	changed = base
	changed.CommitSHA = "89abcdef0123456789abcdef0123456789abcdef"
	if _, err := services.AuthoringLaunches.Start(ctx, changed); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("changed completed commit = %v; want idempotency conflict and one capture", err)
	}
	replayed, err := services.AuthoringLaunches.Start(ctx, base)
	if err != nil || replayed != first || capturer.calls != 1 {
		t.Fatalf("same completed launch replay = %+v err=%v first=%+v calls=%d", replayed, err, first, capturer.calls)
	}
}

func TestStandardAuthoringLaunchRejectsChangedSourceForPreparedKeyBeforeRecapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
		failures:   1,
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "retry source capture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "prepared-source", Title: "Prepared source", MetadataJSON: `{}`,
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err == nil || capturer.calls != 1 {
		t.Fatalf("first source capture = %v; calls=%d", err, capturer.calls)
	}
	changed := command
	changed.CommitSHA = "89abcdef0123456789abcdef0123456789abcdef"
	if _, err := services.AuthoringLaunches.Start(ctx, changed); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("changed prepared source = %v; want idempotency conflict without recapture", err)
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err != nil || capturer.calls != 2 {
		t.Fatalf("same prepared source retry = %v; calls=%d", err, capturer.calls)
	}
}

func TestStandardAuthoringLaunchRejectsDeploymentDriftBeforeRetryCapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.OpenForTest(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first := standardAuthoringLaunchTestDefinitionProvider(t)
	changedProfile := standardAuthoringLaunchTestProfile()
	changedProfile.Version = "2"
	second, err := NewCatalogStandardAuthoringRunDefinitionProvider(first.catalog, changedProfile)
	if err != nil {
		t.Fatal(err)
	}
	definitions := &standardAuthoringMutableDefinitionProvider{current: first}
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate,
		snapshot:   standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
		failures:   1,
	}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, first.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "prove deployment retry fence"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL,
		CommitSHA:                    standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug:                         "deployment-drift-fence", Title: "Deployment drift fence", MetadataJSON: `{}`,
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err == nil || capturer.calls != 1 {
		t.Fatalf("initial capture failure = %v; calls=%d", err, capturer.calls)
	}
	op, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || op == nil || op.State != store.LifecycleOperationPrepared {
		t.Fatalf("prepared lifecycle operation = %+v, %v", op, err)
	}
	operationDirectory, err := services.AuthoringLaunches.openStandardAuthoringLaunchOperationDirectory(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer operationDirectory.Close()
	preparation, found, err := readStandardAuthoringLaunchPreparationAt(operationDirectory)
	if err != nil || !found {
		t.Fatalf("read pre-capture deployment preparation: found=%v err=%v", found, err)
	}
	firstStatic, err := first.StandardAuthoringStaticRunDefinition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint, err := firstStatic.Profile.Fingerprint()
	if err != nil || preparation.ProfileFingerprint != firstFingerprint || preparation.PreparationFingerprint == "" {
		t.Fatalf("preparation did not freeze the first deployment definition: %+v err=%v", preparation, err)
	}

	definitions.current = second
	if _, err := services.AuthoringLaunches.Start(ctx, command); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("deployment-drift retry = %v, want immutable preparation conflict", err)
	}
	if capturer.calls != 1 {
		t.Fatalf("deployment-drift retry contacted Git: calls=%d, want 1", capturer.calls)
	}
	if definitions.runCalls != 0 {
		t.Fatalf("deployment-drift retry built a source-bound definition: calls=%d", definitions.runCalls)
	}
}

func TestStandardAuthoringLaunchPreparationPersistsStaticCatalogLockAndProfileIdentity(t *testing.T) {
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := standardAuthoringLaunchIdentities(key)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	receipt := stageprovider.DeploymentOperationCatalogReceipt{
		Format:               stageprovider.DeploymentOperationCatalogReceiptFormat,
		Version:              stageprovider.DeploymentOperationCatalogReceiptVersion,
		CatalogFormat:        stageprovider.DeploymentOperationCatalogFormat,
		CatalogSchemaVersion: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID:            "standard-authoring-test-catalog",
		CatalogVersion:       "1",
		Template:             workflowadapter.StandardAuthoringTemplateReference(),
		CatalogFingerprint:   workflowkit.SHA256Fingerprint([]byte("catalog")),
	}
	receiptCanonical, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	lockIdentity := &stageprovider.DeploymentOperationCatalogLockIdentity{
		LockID: "standard-authoring-test-lock", LockVersion: "1", Fingerprint: workflowkit.SHA256Fingerprint([]byte("lock")),
	}
	definition, err := newStandardAuthoringLaunchDeploymentDefinitionWithoutResolver(standardAuthoringLaunchTestProfile(), receiptCanonical, lockIdentity)
	if err != nil {
		t.Fatal(err)
	}
	preparation := newStandardAuthoringLaunchPreparation(store.LifecycleOperation{
		ID: operationID, Action: string(standardAuthoringLaunchAction), TaskID: ids.TaskID, RunID: ids.RunID, State: store.LifecycleOperationPrepared,
	}, ids, definition)
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	canonical, err := preparation.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := standardAuthoringWriteNewImmutableFileAt(directory, standardAuthoringLaunchPreparationFileName, canonical, 0o640); err != nil {
		t.Fatal(err)
	}
	stored, found, err := readStandardAuthoringLaunchPreparationAt(directory)
	if err != nil || !found {
		t.Fatalf("read preparation = found=%v err=%v", found, err)
	}
	storedDefinition, err := stored.DeploymentDefinition()
	if err != nil {
		t.Fatal(err)
	}
	if !sameStandardAuthoringLaunchDeploymentDefinition(storedDefinition, definition) ||
		!bytes.Equal(stored.DeploymentCatalogReceipt, receiptCanonical) ||
		!sameDeploymentCatalogLockIdentity(stored.DeploymentCatalogLockIdentity, lockIdentity) ||
		stored.ProfileFingerprint != definition.ProfileFingerprint || stored.PreparationFingerprint != definition.Fingerprint {
		t.Fatalf("persisted static deployment identity = %+v, want %+v", stored, definition)
	}
}

func TestStandardAuthoringLaunchCompletedReplayIgnoresLaterDeploymentAvailabilityAndDrift(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first := standardAuthoringLaunchTestDefinitionProvider(t)
	changedProfile := standardAuthoringLaunchTestProfile()
	changedProfile.Version = "later-deployment-version"
	changed, err := NewCatalogStandardAuthoringRunDefinitionProvider(first.catalog, changedProfile)
	if err != nil {
		t.Fatal(err)
	}
	definitions := &standardAuthoringMutableDefinitionProvider{current: first}
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, first.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "prove completed replay is deployment independent"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "completed-replay-deployment", Title: "Completed replay deployment", MetadataJSON: `{}`,
	}
	firstReceipt, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	definitions.current = changed
	services.AuthoringLaunches.definitions = nil
	replayed, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil || replayed != firstReceipt || capturer.calls != 1 {
		t.Fatalf("completed replay after deployment change = %+v err=%v first=%+v calls=%d", replayed, err, firstReceipt, capturer.calls)
	}
	changedInput := command
	changedInput.Title = "changed user input"
	if _, err := services.AuthoringLaunches.Start(ctx, changedInput); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("changed completed input = %v, want idempotency conflict and no recapture", err)
	}
}

func TestStandardAuthoringLaunchRejectsMissingOrMismatchedStaticCatalogReceiptBeforeCapture(t *testing.T) {
	ctx := context.Background()
	t.Run("missing static provider receipt", func(t *testing.T) {
		// Use one catalog instance for both the provider and core binding. The
		// override only removes the provider-owned static proof.
		provider := standardAuthoringLaunchTestDefinitionProvider(t)
		definitions := &standardAuthoringStaticReceiptOverrideProvider{delegate: provider, staticReceipt: nil}
		standardAuthoringAssertStaticCatalogPreflightFailure(t, ctx, definitions, provider.catalog)
	})

	t.Run("mismatched static provider receipt", func(t *testing.T) {
		provider := standardAuthoringLaunchTestDefinitionProvider(t)
		mismatchedDocument := provider.catalog.Catalog()
		mismatchedDocument.CatalogVersion = "mismatched-static-receipt"
		mismatchedCatalog, err := stageprovider.NewDeploymentOperationCatalogResolver(mismatchedDocument)
		if err != nil {
			t.Fatal(err)
		}
		mismatchedProvider, err := NewCatalogStandardAuthoringRunDefinitionProvider(mismatchedCatalog, standardAuthoringLaunchTestProfile())
		if err != nil {
			t.Fatal(err)
		}
		standardAuthoringAssertStaticCatalogPreflightFailure(t, ctx, mismatchedProvider, provider.catalog)
	})
}

func standardAuthoringAssertStaticCatalogPreflightFailure(t *testing.T, ctx context.Context, definitions StandardAuthoringRunDefinitionProvider, catalog *stageprovider.DeploymentOperationCatalogResolver) {
	t.Helper()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "prove static catalog preflight"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "static-catalog-preflight", Title: "Static catalog preflight", MetadataJSON: `{}`,
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err == nil {
		t.Fatal("static catalog proof unexpectedly started capture")
	}
	if capturer.calls != 0 {
		t.Fatalf("static catalog preflight contacted source capture: calls=%d", capturer.calls)
	}
	operation, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || operation != nil {
		t.Fatalf("static catalog preflight created lifecycle operation: operation=%+v err=%v", operation, err)
	}
}

func TestStandardAuthoringLaunchPreparedOperationWithoutPreparationFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "simulate crash before preparation"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "missing-preparation", Title: "Missing preparation", MetadataJSON: `{}`,
	}
	coordinate, err := standardAuthoringLaunchCoordinate(command)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := canonicalStandardAuthoringMetadata(command.MetadataJSON)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := standardAuthoringLaunchIdentities(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, replay, err := (&LifecycleMutationService{core: services.core}).begin(ctx, standardAuthoringLaunchAction, command.LifecycleMutationCommandBase, standardAuthoringLaunchRequest{
		RepositoryURL: coordinate.RepositoryURL, CommitSHA: coordinate.CommitSHA, Slug: command.Slug, Title: command.Title, MetadataJSON: metadata,
	}, lifecycleOperationTargets{TaskID: ids.TaskID, RunID: ids.RunID}); err != nil || replay != nil {
		t.Fatalf("prepare operation without filesystem definition: replay=%+v err=%v", replay, err)
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); !errors.Is(err, store.ErrIdempotencyConflict) {
		t.Fatalf("missing preparation retry = %v, want idempotency conflict", err)
	}
	if capturer.calls != 0 {
		t.Fatalf("missing preparation retry contacted Git: calls=%d", capturer.calls)
	}
}

func TestStandardAuthoringLaunchRejectsSymlinkedManagedRecordsWithoutRecapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	capturer := &standardAuthoringSourceCapturerFixture{
		coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate), failures: 1,
	}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "reject malformed capture receipt"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "symlinked-capture-record", Title: "Symlinked capture record", MetadataJSON: `{}`,
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err == nil || capturer.calls != 1 {
		t.Fatalf("initial transient capture failure = %v calls=%d", err, capturer.calls)
	}
	op, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || op == nil {
		t.Fatalf("load prepared operation: operation=%+v err=%v", op, err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside-capture-receipt"), filepath.Join(root, managedLifecycleOperationsDirectory, op.ID, standardAuthoringLaunchCaptureReceiptFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); err == nil || capturer.calls != 1 {
		t.Fatalf("symlinked capture receipt retry = %v calls=%d, want rejection without recapture", err, capturer.calls)
	}
}

func TestStandardAuthoringLaunchRootedOperationDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, managedLifecycleOperationsDirectory)); err != nil {
		t.Fatal(err)
	}
	operationID, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	service := &StandardAuthoringLaunchService{core: &lifecycleServiceCore{layout: managedLayout{root: root}}}
	if directory, err := service.openStandardAuthoringLaunchOperationDirectory(operationID); err == nil {
		_ = directory.Close()
		t.Fatal("symlinked operations directory was accepted")
	}
}

func TestStandardAuthoringLaunchRecoveryUsesCaptureReceiptWithoutRecapture(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := standardAuthoringLaunchIdentities(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateTaskV2(ctx, store.CreateTaskV2Request{
		ID: ids.TaskID, Slug: "occupied-recovery-task", Title: "Occupied recovery task", MetadataJSON: `{}`,
		SourceRepo: standardAuthoringLaunchTestCoordinate.RepositoryURL, SourceCommit: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Actor: "author", Reason: "force failure after capture receipt",
	}); err != nil {
		t.Fatal(err)
	}
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	capturer := &standardAuthoringSourceCapturerFixture{coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate)}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "recover durable capture receipt"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "expected-different-task", Title: "Expected different task", MetadataJSON: `{}`,
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("first post-receipt failure = %v calls=%d", err, capturer.calls)
	}
	op, err := database.GetLifecycleOperationByIdempotencyKey(ctx, key)
	if err != nil || op == nil {
		t.Fatalf("load prepared operation: operation=%+v err=%v", op, err)
	}
	directory, err := services.AuthoringLaunches.openStandardAuthoringLaunchOperationDirectory(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	capture, found, err := readStandardAuthoringLaunchCaptureReceiptAt(directory)
	_ = directory.Close()
	if err != nil || !found || capture.SourceSnapshotObject.Digest == "" {
		t.Fatalf("capture receipt = %+v found=%v err=%v", capture, found, err)
	}
	if _, err := services.AuthoringLaunches.Start(ctx, command); !errors.Is(err, store.ErrIdempotencyConflict) || capturer.calls != 1 {
		t.Fatalf("recovery after receipt = %v calls=%d, want no recapture", err, capturer.calls)
	}
}

func TestStandardAuthoringLaunchSerializesCaptureAndCancellationDoesNotPublishAnotherReceipt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	capturer := &standardAuthoringBlockingSourceCapturer{
		coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "serialize same operation capture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "serialized-capture", Title: "Serialized capture", MetadataJSON: `{}`,
	}
	firstResult := make(chan struct {
		receipt LifecycleMutationReceipt
		err     error
	}, 1)
	go func() {
		receipt, err := services.AuthoringLaunches.Start(ctx, command)
		firstResult <- struct {
			receipt LifecycleMutationReceipt
			err     error
		}{receipt: receipt, err: err}
	}()
	select {
	case <-capturer.started:
	case <-time.After(time.Second):
		t.Fatal("first source capture did not start")
	}
	secondContext, cancel := context.WithCancel(ctx)
	secondResult := make(chan error, 1)
	go func() {
		_, err := services.AuthoringLaunches.Start(secondContext, command)
		secondResult <- err
	}()
	time.Sleep(2 * standardAuthoringLaunchLockPollInterval)
	cancel()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled peer launch error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled peer launch did not return")
	}
	if capturer.CallCount() != 1 {
		t.Fatalf("cancelled peer invoked capture: calls=%d", capturer.CallCount())
	}
	close(capturer.release)
	var firstReceipt LifecycleMutationReceipt
	select {
	case result := <-firstResult:
		if result.err != nil {
			t.Fatalf("first serialized launch: %v", result.err)
		}
		firstReceipt = result.receipt
	case <-time.After(time.Second):
		t.Fatal("first serialized launch did not complete")
	}
	replayed, err := services.AuthoringLaunches.Start(ctx, command)
	if err != nil || replayed != firstReceipt || capturer.CallCount() != 1 {
		t.Fatalf("post-cancellation replay = %+v err=%v first=%+v calls=%d", replayed, err, firstReceipt, capturer.CallCount())
	}
}

func TestStandardAuthoringLaunchConcurrentSuccessfulCallsShareOneCaptureReceipt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	definitions := standardAuthoringLaunchTestDefinitionProvider(t)
	capturer := &standardAuthoringBlockingSourceCapturer{
		coordinate: standardAuthoringLaunchTestCoordinate, snapshot: standardAuthoringLaunchTestSnapshot(t, standardAuthoringLaunchTestCoordinate),
		started: make(chan struct{}), release: make(chan struct{}),
	}
	services, err := NewLifecycleServicesWithOptions(root, database, standardAuthoringLaunchTestOptions(capturer, definitions, definitions.catalog))
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.NewUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	command := StandardAuthoringLaunchCommand{
		LifecycleMutationCommandBase: LifecycleMutationCommandBase{IdempotencyKey: key, Actor: "author", Reason: "concurrent successful source capture"},
		RepositoryURL:                standardAuthoringLaunchTestCoordinate.RepositoryURL, CommitSHA: standardAuthoringLaunchTestCoordinate.CommitSHA,
		Slug: "concurrent-success-capture", Title: "Concurrent success capture", MetadataJSON: `{}`,
	}
	type outcome struct {
		receipt LifecycleMutationReceipt
		err     error
	}
	results := make(chan outcome, 2)
	launch := func() {
		receipt, err := services.AuthoringLaunches.Start(ctx, command)
		results <- outcome{receipt: receipt, err: err}
	}
	go launch()
	select {
	case <-capturer.started:
	case <-time.After(time.Second):
		t.Fatal("first concurrent capture did not start")
	}
	go launch()
	// The first launch still owns the real operation flock while Git capture is
	// blocked. Give the peer a scheduling turn before permitting capture to
	// finish, then require both callers to observe the same durable receipt.
	time.Sleep(2 * standardAuthoringLaunchLockPollInterval)
	close(capturer.release)
	var outcomes [2]outcome
	for index := range outcomes {
		select {
		case outcomes[index] = <-results:
		case <-time.After(time.Second):
			t.Fatal("concurrent successful launch did not complete")
		}
	}
	first, second := outcomes[0], outcomes[1]
	if first.err != nil || second.err != nil || first.receipt != second.receipt || capturer.CallCount() != 1 {
		t.Fatalf("concurrent launch results first=%+v/%v second=%+v/%v capture-calls=%d", first.receipt, first.err, second.receipt, second.err, capturer.CallCount())
	}
}

type standardAuthoringSourceCapturerFixture struct {
	coordinate StandardAuthoringSourceCoordinate
	snapshot   StandardAuthoringSourceSnapshot
	calls      int
	failures   int
}

type standardAuthoringStaticReceiptOverrideProvider struct {
	delegate      StandardAuthoringRunDefinitionProvider
	staticReceipt []byte
}

func (provider *standardAuthoringStaticReceiptOverrideProvider) StandardAuthoringStaticRunDefinition(ctx context.Context) (StandardAuthoringStaticRunDefinition, error) {
	staticProvider, ok := provider.delegate.(StandardAuthoringStaticRunDefinitionProvider)
	if !ok || staticProvider == nil {
		return StandardAuthoringStaticRunDefinition{}, ErrStandardAuthoringLaunchUnavailable
	}
	definition, err := staticProvider.StandardAuthoringStaticRunDefinition(ctx)
	if err != nil {
		return StandardAuthoringStaticRunDefinition{}, err
	}
	definition.DeploymentCatalogReceipt = append([]byte(nil), provider.staticReceipt...)
	return definition, nil
}

func (provider *standardAuthoringStaticReceiptOverrideProvider) StandardAuthoringRunDefinition(ctx context.Context, subject StandardAuthoringRunDefinitionSubject) (StandardAuthoringRunDefinition, error) {
	return provider.delegate.StandardAuthoringRunDefinition(ctx, subject)
}

type standardAuthoringBlockingSourceCapturer struct {
	coordinate StandardAuthoringSourceCoordinate
	snapshot   StandardAuthoringSourceSnapshot
	started    chan struct{}
	release    chan struct{}
	once       sync.Once
	mu         sync.Mutex
	calls      int
}

func (capturer *standardAuthoringBlockingSourceCapturer) CaptureStandardAuthoringSource(ctx context.Context, coordinate StandardAuthoringSourceCoordinate) (StandardAuthoringSourceSnapshot, error) {
	if coordinate != capturer.coordinate {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("unexpected source coordinate: %+v", coordinate)
	}
	capturer.mu.Lock()
	capturer.calls++
	capturer.mu.Unlock()
	capturer.once.Do(func() { close(capturer.started) })
	select {
	case <-ctx.Done():
		return StandardAuthoringSourceSnapshot{}, ctx.Err()
	case <-capturer.release:
	}
	result := capturer.snapshot
	result.Content = append([]byte(nil), capturer.snapshot.Content...)
	return result, nil
}

func (capturer *standardAuthoringBlockingSourceCapturer) CallCount() int {
	capturer.mu.Lock()
	defer capturer.mu.Unlock()
	return capturer.calls
}

func standardAuthoringLaunchTestOptions(capturer StandardAuthoringSourceCapturer, definitions StandardAuthoringRunDefinitionProvider, catalog *stageprovider.DeploymentOperationCatalogResolver) LifecycleServicesOptions {
	return LifecycleServicesOptions{
		OperationResolver: testsupport.AcceptAllStageOperationResolver(),
		DeploymentCatalogResolvers: []TemplateDeploymentCatalogResolver{{
			Template: workflowadapter.StandardAuthoringTemplateReference(), Resolver: catalog,
		}},
		StandardAuthoringSourceCapturer:        capturer,
		StandardAuthoringRunDefinitionProvider: definitions,
	}
}

type standardAuthoringMutableDefinitionProvider struct {
	current     *CatalogStandardAuthoringRunDefinitionProvider
	staticCalls int
	runCalls    int
}

func (provider *standardAuthoringMutableDefinitionProvider) StandardAuthoringStaticRunDefinition(ctx context.Context) (StandardAuthoringStaticRunDefinition, error) {
	provider.staticCalls++
	return provider.current.StandardAuthoringStaticRunDefinition(ctx)
}

func (provider *standardAuthoringMutableDefinitionProvider) StandardAuthoringRunDefinition(ctx context.Context, subject StandardAuthoringRunDefinitionSubject) (StandardAuthoringRunDefinition, error) {
	provider.runCalls++
	return provider.current.StandardAuthoringRunDefinition(ctx, subject)
}

func (fixture *standardAuthoringSourceCapturerFixture) CaptureStandardAuthoringSource(_ context.Context, coordinate StandardAuthoringSourceCoordinate) (StandardAuthoringSourceSnapshot, error) {
	if coordinate != fixture.coordinate {
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("unexpected source coordinate: %+v", coordinate)
	}
	fixture.calls++
	if fixture.failures > 0 {
		fixture.failures--
		return StandardAuthoringSourceSnapshot{}, fmt.Errorf("transient source capture failure")
	}
	result := fixture.snapshot
	result.Content = append([]byte(nil), fixture.snapshot.Content...)
	return result, nil
}

func standardAuthoringLaunchTestSnapshot(t *testing.T, coordinate StandardAuthoringSourceCoordinate) StandardAuthoringSourceSnapshot {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: standardAuthoringGitPAXGlobalHeaderName, Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": coordinate.CommitSHA}}); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"source/Cargo.toml": "[package]\nname = \"fixture\"\n",
		"source/src/lib.rs": "pub fn fixture() {}\n",
	} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return StandardAuthoringSourceSnapshot{RepositoryURL: coordinate.RepositoryURL, CommitSHA: coordinate.CommitSHA, SchemaVersion: StandardAuthoringSourceSnapshotSchemaVersion, Content: archive.Bytes()}
}

func standardAuthoringLaunchTestDefinitionProvider(t *testing.T) *CatalogStandardAuthoringRunDefinitionProvider {
	t.Helper()
	catalogDocument := stageprovider.DeploymentOperationCatalog{
		Format: stageprovider.DeploymentOperationCatalogFormat, Version: stageprovider.DeploymentOperationCatalogVersion,
		CatalogID: "standard-authoring-test", CatalogVersion: "1", Template: workflowadapter.StandardAuthoringTemplateReference(), Operations: []stageprovider.DeploymentOperationRegistration{},
	}
	for _, stage := range workflowadapter.StandardAuthoringStageCatalog().Stages {
		operation := workflowadapter.StageOperationBinding{ProviderID: "standard-authoring-test-provider", OperationID: "test." + string(stage.Key), Version: "1"}
		switch stage.Key {
		case workflowkit.StageKey(workflowadapter.RepoPrepare):
			operation.Payload = workflowadapter.LocalCommandOperationPayload{CommandID: "test-source-capture", Arguments: []string{}}
		case workflowkit.StageKey(workflowadapter.TaskReview), workflowkit.StageKey(workflowadapter.ContentReview), workflowkit.StageKey(workflowadapter.SolutionReview):
			operation.Payload = workflowadapter.DurableReviewOperationPayload{PolicyID: "test-review"}
		case workflowkit.StageKey(workflowadapter.MaterializeTask):
			operation.Payload = workflowadapter.HarborBuiltinOperationPayload{HandlerID: "test-materialize"}
		default:
			operation.Payload = workflowadapter.AgentTurnOperationPayload{AgentID: "test-agent", ModelID: "test-model", ReasoningEffort: workflowadapter.AgentReasoningEffortHigh, MaxTurns: stage.RequiredTurns}
		}
		catalogDocument.Operations = append(catalogDocument.Operations, stageprovider.DeploymentOperationRegistration{
			Stage:     stageprovider.DeploymentStageContract{Key: stage.Key, Type: standardAuthoringLaunchTestStageType(t, stage.Key), Group: stage.Group, Plugin: workflowkit.PluginBinding{ID: stage.Plugin.ID, Version: stage.Plugin.Version}},
			Provider:  workflowadapter.ProviderReference{ID: "standard-authoring-test-provider", Kind: "test", Version: "1"},
			Operation: operation, Runtime: workflowadapter.RuntimeReference{ID: "standard-authoring-test-runtime", Kind: "test", Version: "1"},
			Checkout: stageprovider.DeploymentCheckoutContract{ID: "standard-authoring-test-checkout", Purpose: "source"}, Secrets: []workflowadapter.SecretReference{},
		})
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		t.Fatalf("build test Standard authoring catalog: %v", err)
	}
	provider, err := NewCatalogStandardAuthoringRunDefinitionProvider(catalog, standardAuthoringLaunchTestProfile())
	if err != nil {
		t.Fatalf("build test Standard authoring definition provider: %v", err)
	}
	return provider
}

func standardAuthoringLaunchTestStageType(t *testing.T, key workflowkit.StageKey) workflowadapter.StageBindingType {
	t.Helper()
	switch key {
	case workflowkit.StageKey(workflowadapter.RepoPrepare):
		return workflowadapter.StageBindingRepoPrepare
	case workflowkit.StageKey(workflowadapter.RepoAnalyze):
		return workflowadapter.StageBindingRepoAnalyze
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return workflowadapter.StageBindingTaskDesign
	case workflowkit.StageKey(workflowadapter.TaskReview):
		return workflowadapter.StageBindingTaskReview
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return workflowadapter.StageBindingGenerateTaskFiles
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		return workflowadapter.StageBindingInstructionGen
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		return workflowadapter.StageBindingTaskTOMLGen
	case workflowkit.StageKey(workflowadapter.DockerfileGen):
		return workflowadapter.StageBindingDockerfileGen
	case workflowkit.StageKey(workflowadapter.ContentReview):
		return workflowadapter.StageBindingContentReview
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return workflowadapter.StageBindingSolveGen
	case workflowkit.StageKey(workflowadapter.TestGen):
		return workflowadapter.StageBindingTestGen
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return workflowadapter.StageBindingTestsAnalysis
	case workflowkit.StageKey(workflowadapter.SolutionReview):
		return workflowadapter.StageBindingSolutionReview
	case workflowkit.StageKey(workflowadapter.MaterializeTask):
		return workflowadapter.StageBindingMaterializeTask
	default:
		t.Fatalf("unsupported Standard authoring test stage %q", key)
		return ""
	}
}

func standardAuthoringLaunchTestProfile() workflowadapter.ExecutionProfile {
	template := workflowadapter.StandardAuthoringWorkflowTemplate()
	profile := workflowadapter.ExecutionProfile{
		Template: template.Reference(), ID: "standard-authoring-test", Version: "1", ContinuationPlanTTL: workflowadapter.RequiredContinuationPlanTTL,
		ControlGracePeriod: 30 * time.Second, CandidateProviderBudget: workflowadapter.CandidateProviderBudget{AttemptTimeout: time.Minute},
	}
	for _, stage := range template.Catalog.Stages {
		turns := max(1, stage.RequiredTurns)
		profile.Stages = append(profile.Stages, workflowadapter.StageBudget{StageKey: stage.Key, Budget: workflowkit.ExecutionBudget{
			TurnTimeout: time.Second, MaxTurns: turns, AttemptTimeout: time.Duration(turns) * time.Second, MaxAttempts: 1, MaxElapsed: time.Duration(turns) * time.Second,
		}})
	}
	if err := profile.Validate(); err != nil {
		panic(fmt.Sprintf("invalid Standard authoring test profile: %v", err))
	}
	return profile
}
