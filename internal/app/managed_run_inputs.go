package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const managedTaskSnapshotInputPort = "task_snapshot"

type managedRunInputMaterialization struct {
	input  runManifestManagedInput
	object workflowruntime.ObjectRef
}

// runManifestManagedInput is the immutable input proof embedded alongside the
// final execution spec. The durable run_input_artifacts row repeats these
// facts, so recovery can detect a partial StartRun without trusting a path or
// manufacturing a StageAttempt.
type runManifestManagedInput struct {
	ID             string                    `json:"id"`
	Port           string                    `json:"port"`
	ContentDigest  workflowkit.Fingerprint   `json:"content_digest"`
	SchemaVersion  string                    `json:"schema_version"`
	SizeBytes      int64                     `json:"size_bytes"`
	RevisionDigest workflowkit.SubjectDigest `json:"revision_digest"`
}

func (input runManifestManagedInput) validate() error {
	if err := store.ValidateUUIDv7(input.ID); err != nil {
		return fmt.Errorf("managed run input ID: %w", err)
	}
	if strings.TrimSpace(input.Port) == "" || strings.TrimSpace(input.SchemaVersion) == "" {
		return fmt.Errorf("managed run input port and schema version are required")
	}
	if err := input.ContentDigest.Validate(); err != nil {
		return fmt.Errorf("managed run input content digest: %w", err)
	}
	if err := input.RevisionDigest.Validate(); err != nil {
		return fmt.Errorf("managed run input revision digest: %w", err)
	}
	if input.SizeBytes < 0 {
		return fmt.Errorf("managed run input size cannot be negative")
	}
	return nil
}

func (input runManifestManagedInput) objectRef() workflowruntime.ObjectRef {
	return workflowruntime.ObjectRef{Digest: input.ContentDigest, SizeBytes: input.SizeBytes}
}

func (input runManifestManagedInput) artifactReference() workflowadapter.ArtifactReference {
	return workflowadapter.ArtifactReference{
		ID: workflowkit.ArtifactID(input.ID), ContentDigest: input.ContentDigest, SchemaVersion: input.SchemaVersion,
	}
}

// prepareManagedInitialRunInputs materializes only intrinsic subject inputs:
// catalog inputs with consumers but no workflow producer. task_snapshot is
// the first registered materializer. Future input kinds add an explicit
// descriptor here rather than receiving a caller path or a generic fallback.
func (service *RunService) prepareManagedInitialRunInputs(ctx context.Context, runID string, revision store.TaskRevision, specification workflowadapter.RunExecutionSpec, recovered []runManifestManagedInput) (workflowadapter.RunExecutionSpec, []runManifestManagedInput, error) {
	archiveTimestamp, err := managedTaskSnapshotArchiveTimestamp(service.core, revision)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, err
	}
	return service.prepareManagedInitialRunInputsAt(ctx, runID, revision, specification, recovered, archiveTimestamp)
}

// prepareManagedInitialRunInputsAt is the shared materialization/binding path
// for normal StartRun and a pre-commit candidate child Run. Candidate snapshots
// exist before their TaskRevision row, so their immutable revision manifest
// supplies this timestamp rather than a mutable wall-clock value.
func (service *RunService) prepareManagedInitialRunInputsAt(ctx context.Context, runID string, revision store.TaskRevision, specification workflowadapter.RunExecutionSpec, recovered []runManifestManagedInput, archiveTimestamp time.Time) (workflowadapter.RunExecutionSpec, []runManifestManagedInput, error) {
	if service == nil || service.core == nil || service.core.objects == nil {
		return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("managed run input materializer is not configured")
	}
	if archiveTimestamp.IsZero() {
		return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("managed task snapshot archive timestamp is required")
	}
	requiredPorts, err := intrinsicTemplateInputPorts(specification, []string{managedTaskSnapshotInputPort, workflowadapter.CodeEdgeSourceSnapshotArtifact})
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, err
	}
	if len(requiredPorts) == 0 {
		if len(recovered) != 0 {
			return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("recovered run manifest declares unsupported managed inputs")
		}
		return specification.Clone(), nil, nil
	}

	recoveredByPort, err := recoveredManagedInputsByPort(recovered, requiredPorts, revision)
	if err != nil {
		return workflowadapter.RunExecutionSpec{}, nil, err
	}
	materialized := make([]managedRunInputMaterialization, 0, len(requiredPorts))
	for _, port := range requiredPorts {
		input, found := recoveredByPort[port]
		if !found {
			id, err := store.NewUUIDv7()
			if err != nil {
				return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("allocate managed %s input ID: %w", port, err)
			}
			input = runManifestManagedInput{
				ID: id, Port: port, SchemaVersion: managedRunInputSchemaVersion(port),
				RevisionDigest: workflowkit.SubjectDigest(revision.TaskDigest),
			}
		}
		object, err := materializeManagedRunInputObjectAt(ctx, service.core, revision, archiveTimestamp, port)
		if err != nil {
			return workflowadapter.RunExecutionSpec{}, nil, err
		}
		if found {
			if input.ContentDigest != object.Digest || input.SizeBytes != object.SizeBytes || input.SchemaVersion != managedRunInputSchemaVersion(port) {
				return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("recovered managed %s object does not match deterministic revision input", port)
			}
		} else {
			input.ContentDigest = object.Digest
			input.SizeBytes = object.SizeBytes
		}
		materialized = append(materialized, managedRunInputMaterialization{input: input, object: object})
	}
	bound := specification.Clone()
	inputs := make([]runManifestManagedInput, 0, len(materialized))
	for _, item := range materialized {
		next, err := bound.BindManagedArtifactInput(item.input.Port, item.input.artifactReference())
		if err != nil {
			return workflowadapter.RunExecutionSpec{}, nil, fmt.Errorf("bind managed %s input: %w", item.input.Port, err)
		}
		bound = next
		inputs = append(inputs, item.input)
	}
	sort.Slice(inputs, func(left, right int) bool { return inputs[left].Port < inputs[right].Port })
	return bound, inputs, nil
}

func intrinsicTemplateInputPort(specification workflowadapter.RunExecutionSpec, port string) (bool, error) {
	ports, err := intrinsicTemplateInputPorts(specification, []string{port})
	if err != nil {
		return false, err
	}
	return len(ports) == 1, nil
}

func intrinsicTemplateInputPorts(specification workflowadapter.RunExecutionSpec, candidates []string) ([]string, error) {
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		return nil, err
	}
	candidateSet := make(map[string]struct{}, len(candidates))
	for _, port := range candidates {
		candidateSet[port] = struct{}{}
	}
	consumers := make(map[string]struct{}, len(candidates))
	for _, stage := range template.Catalog.Stages {
		for _, output := range stage.Outputs {
			if _, tracked := candidateSet[output.Name]; tracked {
				delete(consumers, output.Name)
				delete(candidateSet, output.Name)
			}
		}
		for _, input := range stage.Inputs {
			if _, tracked := candidateSet[input.Name]; tracked {
				consumers[input.Name] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(consumers))
	for port := range consumers {
		result = append(result, port)
	}
	sort.Strings(result)
	return result, nil
}

func recoveredManagedInputsByPort(recovered []runManifestManagedInput, requiredPorts []string, revision store.TaskRevision) (map[string]runManifestManagedInput, error) {
	required := make(map[string]struct{}, len(requiredPorts))
	for _, port := range requiredPorts {
		required[port] = struct{}{}
	}
	if len(recovered) == 0 {
		return nil, nil
	}
	if len(recovered) != len(required) {
		return nil, fmt.Errorf("recovered run manifest has invalid managed input set")
	}
	result := make(map[string]runManifestManagedInput, len(recovered))
	for _, input := range recovered {
		if err := input.validate(); err != nil {
			return nil, err
		}
		if _, wanted := required[input.Port]; !wanted {
			return nil, fmt.Errorf("recovered run manifest declares unsupported managed input %q", input.Port)
		}
		if _, duplicate := result[input.Port]; duplicate {
			return nil, fmt.Errorf("recovered run manifest has duplicate managed input port %q", input.Port)
		}
		if input.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) {
			return nil, fmt.Errorf("recovered managed %s does not match selected TaskRevision", input.Port)
		}
		result[input.Port] = input
	}
	for port := range required {
		if _, found := result[port]; !found {
			return nil, fmt.Errorf("recovered run manifest is missing managed input %q", port)
		}
	}
	return result, nil
}

func managedRunInputSchemaVersion(port string) string {
	switch port {
	case managedTaskSnapshotInputPort:
		return "harbor.artifact.v1"
	case workflowadapter.CodeEdgeSourceSnapshotArtifact:
		return workflowadapter.CodeEdgeSourceSnapshotSchemaVersion
	default:
		return ""
	}
}

func materializeManagedRunInputObjectAt(ctx context.Context, core *lifecycleServiceCore, revision store.TaskRevision, archiveTimestamp time.Time, port string) (workflowruntime.ObjectRef, error) {
	switch port {
	case managedTaskSnapshotInputPort:
		return materializeManagedTaskSnapshotObjectAt(ctx, core, revision, archiveTimestamp)
	case workflowadapter.CodeEdgeSourceSnapshotArtifact:
		return materializeManagedSourceSnapshotObject(ctx, core, revision)
	default:
		return workflowruntime.ObjectRef{}, fmt.Errorf("unsupported managed input port %q", port)
	}
}

// materializeManagedTaskSnapshotObject writes the sealed revision snapshot as
// deterministic ZIP bytes. CanonicalFiles supplies lexical entry ordering and
// fixed modes; the immutable revision creation time supplies the only ZIP
// timestamp. taskpolicy validates both before the archive is read and after
// the revision digest is recomputed, rejecting symlinks and non-regular files.
func materializeManagedTaskSnapshotObject(ctx context.Context, core *lifecycleServiceCore, revision store.TaskRevision) (workflowruntime.ObjectRef, error) {
	archiveTimestamp, err := managedTaskSnapshotArchiveTimestamp(core, revision)
	if err != nil {
		return workflowruntime.ObjectRef{}, err
	}
	return materializeManagedTaskSnapshotObjectAt(ctx, core, revision, archiveTimestamp)
}

func managedTaskSnapshotArchiveTimestamp(core *lifecycleServiceCore, revision store.TaskRevision) (time.Time, error) {
	if core == nil {
		return time.Time{}, fmt.Errorf("managed task snapshot layout is not configured")
	}
	raw, err := os.ReadFile(core.layout.revisionManifestPath(revision.TaskID, revision.ID))
	if err != nil {
		return time.Time{}, fmt.Errorf("read sealed task revision manifest: %w", err)
	}
	var manifest revisionSnapshotManifest
	if err := decodeStrictJSON(string(raw), &manifest); err != nil {
		return time.Time{}, fmt.Errorf("decode sealed task revision manifest: %w", err)
	}
	if manifest.Format != "harbor.task-revision-manifest.v2" || manifest.TaskID != revision.TaskID || manifest.RevisionID != revision.ID || manifest.TaskDigest != revision.TaskDigest || manifest.SnapshotPath != "snapshot" || manifest.CreatedAt.IsZero() {
		return time.Time{}, fmt.Errorf("sealed task revision manifest does not match immutable revision")
	}
	return manifest.CreatedAt.UTC(), nil
}

func materializeManagedTaskSnapshotObjectAt(ctx context.Context, core *lifecycleServiceCore, revision store.TaskRevision, archiveTimestamp time.Time) (workflowruntime.ObjectRef, error) {
	if core == nil || core.objects == nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed task snapshot object store is not configured")
	}
	if archiveTimestamp.IsZero() {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed task snapshot archive timestamp is required")
	}
	snapshot := core.layout.snapshotDirectory(revision.TaskID, revision.ID)
	if err := taskpolicy.ValidateManagedSnapshotV2(snapshot); err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("validate sealed task snapshot: %w", err)
	}
	digest, err := taskpolicy.ComputeManagedTaskDigestV2(snapshot)
	if err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("digest sealed task snapshot: %w", err)
	}
	if digest != revision.TaskDigest {
		return workflowruntime.ObjectRef{}, fmt.Errorf("sealed task snapshot digest does not match TaskRevision")
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	writeErr := writeCanonicalPackageZip(ctx, writer, snapshot, "task", archiveTimestamp.UTC())
	if closeErr := writer.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("archive sealed task snapshot: %w", writeErr)
	}
	object, err := core.objects.PutBytes(ctx, archive.Bytes())
	if err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("store sealed task snapshot archive: %w", err)
	}
	return object, nil
}

func materializeManagedSourceSnapshotObject(ctx context.Context, core *lifecycleServiceCore, revision store.TaskRevision) (workflowruntime.ObjectRef, error) {
	if core == nil || core.store == nil || core.objects == nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed source snapshot object store is not configured")
	}
	source, err := managedSourceSnapshotAuthoringSource(ctx, core, revision)
	if err != nil {
		return workflowruntime.ObjectRef{}, err
	}
	if source == nil ||
		source.SnapshotArtifactRef != source.SnapshotContentDigest || source.SnapshotSchemaVersion != workflowadapter.CodeEdgeSourceSnapshotSchemaVersion {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed source_snapshot source record does not match the materialized TaskRevision")
	}
	if _, err := standardAuthoringStoredSourceCoordinate(*source); err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed source_snapshot source coordinate: %w", err)
	}
	reference := workflowruntime.ObjectRef{Digest: workflowkit.Fingerprint(source.SnapshotContentDigest)}
	if err := reference.Digest.Validate(); err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed source_snapshot digest: %w", err)
	}
	file, err := core.objects.Open(ctx, reference)
	if err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("open managed source_snapshot object: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return workflowruntime.ObjectRef{}, fmt.Errorf("read managed source_snapshot object: %w", err)
	}
	if actual := workflowkit.Fingerprint("sha256:" + hex.EncodeToString(hash.Sum(nil))); actual != reference.Digest {
		return workflowruntime.ObjectRef{}, fmt.Errorf("managed source_snapshot object digest differs from AuthoringSource")
	}
	return workflowruntime.ObjectRef{Digest: reference.Digest, SizeBytes: size}, nil
}

func managedSourceSnapshotAuthoringSource(ctx context.Context, core *lifecycleServiceCore, revision store.TaskRevision) (*store.AuthoringSource, error) {
	if revision.ID == "" || revision.TaskID == "" || revision.TaskDigest == "" {
		return nil, fmt.Errorf("managed source_snapshot requires a complete TaskRevision subject")
	}
	originRevision := revision
	visited := map[string]struct{}{}
	for {
		if _, duplicate := visited[revision.ID]; duplicate {
			return nil, fmt.Errorf("managed source_snapshot revision lineage contains a cycle at %s", revision.ID)
		}
		visited[revision.ID] = struct{}{}
		materialization, err := core.store.GetAuthoringTaskMaterializationForRevision(ctx, revision.ID)
		if err != nil {
			return nil, err
		}
		if materialization != nil {
			if materialization.RevisionID != revision.ID || materialization.TaskID != revision.TaskID || materialization.TaskDigest != revision.TaskDigest {
				return nil, fmt.Errorf("managed source_snapshot materialization does not match TaskRevision %s", revision.ID)
			}
			source, err := core.store.GetAuthoringSource(ctx, materialization.SourceID)
			if err != nil {
				return nil, err
			}
			if source == nil || source.ID != materialization.SourceID || source.SourceFingerprint != materialization.SourceFingerprint {
				return nil, fmt.Errorf("managed source_snapshot source record does not match the materialized TaskRevision")
			}
			return source, nil
		}
		if revision.ParentRevisionID == "" {
			return nil, fmt.Errorf("managed source_snapshot requires a Standard authoring materialization for TaskRevision %s", originRevision.ID)
		}
		parent, err := core.store.GetTaskRevision(ctx, revision.ParentRevisionID)
		if err != nil {
			return nil, err
		}
		if parent == nil || parent.TaskID != originRevision.TaskID {
			return nil, fmt.Errorf("managed source_snapshot parent revision is missing for TaskRevision %s", revision.ID)
		}
		revision = *parent
	}
}

func (service *RunService) ensureRunInputArtifacts(ctx context.Context, run store.WorkflowRun, manifest runManifest) error {
	if service == nil || service.core == nil || manifest.Inputs == nil {
		return fmt.Errorf("managed run input persistence is not configured")
	}
	for _, input := range manifest.Inputs.ManagedInputs {
		if err := input.validate(); err != nil {
			return err
		}
		existing, err := service.core.store.GetRunInputArtifactForPort(ctx, run.ID, input.Port)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.ID != input.ID || existing.TaskID != run.TaskID || existing.RevisionID != run.RevisionID ||
				existing.RevisionDigest != string(input.RevisionDigest) || existing.ContentDigest != string(input.ContentDigest) ||
				existing.SchemaVersion != input.SchemaVersion || existing.SizeBytes != input.SizeBytes {
				return fmt.Errorf("persisted managed run input %q does not match frozen run manifest", input.Port)
			}
			continue
		}
		created, err := service.core.store.CreateRunInputArtifact(ctx, store.CreateRunInputArtifactRequest{
			ID: input.ID, RunID: run.ID, TaskID: run.TaskID, RevisionID: run.RevisionID,
			RevisionDigest: string(input.RevisionDigest), Port: input.Port,
			ContentDigest: string(input.ContentDigest), SchemaVersion: input.SchemaVersion, SizeBytes: input.SizeBytes,
			IdempotencyKey: "run-input-artifact:" + run.ID + ":" + input.Port,
			Actor:          run.CreatedBy, Reason: "persist sealed managed run input",
		})
		if err != nil {
			return err
		}
		if created.ID != input.ID || created.ContentDigest != string(input.ContentDigest) || created.SizeBytes != input.SizeBytes {
			return fmt.Errorf("created managed run input %q does not match frozen run manifest", input.Port)
		}
	}
	return nil
}

// verifyManagedRunInputs proves the three-way immutable contract used at
// execution time: manifest input declaration, durable run_input_artifacts
// row, and content-addressed object must all agree with the final frozen spec.
func verifyManagedRunInputs(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, revision store.TaskRevision, manifest runManifest, specification workflowadapter.RunExecutionSpec) error {
	if core == nil || core.store == nil || core.objects == nil || manifest.Inputs == nil {
		return fmt.Errorf("managed run input verifier is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	references := make(map[workflowkit.ArtifactID]workflowadapter.ArtifactReference, len(specification.References.Artifacts))
	for _, reference := range specification.References.Artifacts {
		references[reference.ID] = reference
	}
	template, err := workflowadapter.ResolveWorkflowTemplate(specification.Template)
	if err != nil {
		return err
	}
	bindingsByPort := make(map[string][]workflowkit.ArtifactID)
	for _, stage := range template.Catalog.Stages {
		resolution, err := specification.ResolveStageOperation(stage.Key)
		if err != nil {
			return fmt.Errorf("resolve frozen stage %q managed inputs: %w", stage.Key, err)
		}
		for _, binding := range resolution.ArtifactInputs {
			bindingsByPort[binding.Port] = append(bindingsByPort[binding.Port], binding.ArtifactID)
		}
	}
	seenPorts := make(map[string]struct{}, len(manifest.Inputs.ManagedInputs))
	seenIDs := make(map[string]struct{}, len(manifest.Inputs.ManagedInputs))
	for _, input := range manifest.Inputs.ManagedInputs {
		if err := input.validate(); err != nil {
			return err
		}
		if _, duplicate := seenPorts[input.Port]; duplicate {
			return fmt.Errorf("run manifest has duplicate managed input port %q", input.Port)
		}
		if _, duplicate := seenIDs[input.ID]; duplicate {
			return fmt.Errorf("run manifest has duplicate managed input ID %q", input.ID)
		}
		seenPorts[input.Port] = struct{}{}
		seenIDs[input.ID] = struct{}{}
		if input.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) {
			return fmt.Errorf("managed input %q revision digest does not match TaskRevision", input.Port)
		}
		reference, present := references[workflowkit.ArtifactID(input.ID)]
		if !present || reference.ContentDigest != input.ContentDigest || reference.SchemaVersion != input.SchemaVersion {
			return fmt.Errorf("managed input %q does not match final execution specification artifact reference", input.Port)
		}
		bindings := bindingsByPort[input.Port]
		if len(bindings) == 0 {
			return fmt.Errorf("managed input %q is not consumed by the final execution specification", input.Port)
		}
		for _, artifactID := range bindings {
			if artifactID != workflowkit.ArtifactID(input.ID) {
				return fmt.Errorf("managed input %q binding does not use its declared artifact ID", input.Port)
			}
		}
		persisted, err := core.store.GetRunInputArtifact(ctx, input.ID)
		if err != nil {
			return err
		}
		if persisted == nil || persisted.RunID != run.ID || persisted.TaskID != run.TaskID || persisted.RevisionID != run.RevisionID ||
			persisted.RevisionDigest != string(input.RevisionDigest) || persisted.Port != input.Port || persisted.ContentDigest != string(input.ContentDigest) ||
			persisted.SchemaVersion != input.SchemaVersion || persisted.SizeBytes != input.SizeBytes {
			return fmt.Errorf("managed input %q durable record does not match frozen run manifest", input.Port)
		}
		byPort, err := core.store.GetRunInputArtifactForPort(ctx, run.ID, input.Port)
		if err != nil {
			return err
		}
		if byPort == nil || byPort.ID != input.ID {
			return fmt.Errorf("managed input %q durable port binding does not match frozen run manifest", input.Port)
		}
		if err := core.objects.Verify(ctx, input.objectRef()); err != nil {
			return fmt.Errorf("verify managed input %q object: %w", input.Port, err)
		}
	}
	return nil
}

func initialManagedRunInputArtifactRequests(runID, taskID, revisionID, revisionDigest, actor string, inputs []runManifestManagedInput) ([]store.CreateRunInputArtifactRequest, error) {
	requests := make([]store.CreateRunInputArtifactRequest, 0, len(inputs))
	for _, input := range inputs {
		if err := input.validate(); err != nil {
			return nil, err
		}
		if input.RevisionDigest != workflowkit.SubjectDigest(revisionDigest) {
			return nil, fmt.Errorf("managed input %q revision digest does not match selected TaskRevision", input.Port)
		}
		requests = append(requests, store.CreateRunInputArtifactRequest{
			ID: input.ID, RunID: runID, TaskID: taskID, RevisionID: revisionID,
			RevisionDigest: string(input.RevisionDigest), Port: input.Port,
			ContentDigest: string(input.ContentDigest), SchemaVersion: input.SchemaVersion, SizeBytes: input.SizeBytes,
			IdempotencyKey: "run-input-artifact:" + runID + ":" + input.Port,
			Actor:          actor, Reason: "persist sealed managed run input",
		})
	}
	return requests, nil
}

func initialWorkflowRunDispatch(runID, definitionHash string, manifest runManifest) (store.WorkflowRunDispatchRequest, workflowRunExecutionPayload, error) {
	if manifest.Inputs == nil {
		return store.WorkflowRunDispatchRequest{}, workflowRunExecutionPayload{}, fmt.Errorf("managed workflow run dispatch is not configured")
	}
	payload := workflowRunExecutionPayload{
		Format: workflowRunExecutionPayloadFormat, RunID: runID, DefinitionHash: definitionHash,
		ExecutionSpecFingerprint: manifest.Inputs.ExecutionSpecFingerprint, QuotaPolicy: manifest.Resolved.QuotaPolicy.Clone(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return store.WorkflowRunDispatchRequest{}, workflowRunExecutionPayload{}, fmt.Errorf("encode initial workflow run dispatch: %w", err)
	}
	return store.WorkflowRunDispatchRequest{
		CommandType: "workflow_run.execute", PayloadJSON: string(encoded),
		IdempotencyKey: "workflow-run-execution:" + runID,
	}, payload, nil
}

// ensureInitialWorkflowRunDispatch is retained only for idempotent recovery
// of an older partially persisted Run. New StartRun calls insert inputs, Run,
// dispatch job, and outbox in one Store transaction.
func (service *RunService) ensureInitialWorkflowRunDispatch(ctx context.Context, run store.WorkflowRun, manifest runManifest) error {
	if service == nil || service.core == nil {
		return fmt.Errorf("managed workflow run dispatch is not configured")
	}
	dispatch, payload, err := initialWorkflowRunDispatch(run.ID, run.DefinitionHash, manifest)
	if err != nil {
		return err
	}
	existing, err := service.core.store.GetDurableJobByIdempotency(ctx, dispatch.IdempotencyKey)
	if err != nil {
		return err
	}
	if existing != nil {
		var actual workflowRunExecutionPayload
		if err := decodeStrictJSON(existing.PayloadJSON, &actual); err != nil ||
			existing.CommandType != "workflow_run.execute" || existing.EntityType != "workflow_run" || existing.EntityID != run.ID || existing.RunID != run.ID ||
			actual.Format != workflowRunExecutionPayloadFormat || actual.RunID != run.ID || actual.DefinitionHash != run.DefinitionHash ||
			actual.ExecutionSpecFingerprint != payload.ExecutionSpecFingerprint || !reflect.DeepEqual(actual.QuotaPolicy, payload.QuotaPolicy) {
			return fmt.Errorf("existing initial workflow run dispatch does not match frozen run manifest")
		}
		return nil
	}
	created, err := service.core.store.CreateDurableJob(ctx, store.CreateDurableJobRequest{
		CommandType: "workflow_run.execute", EntityType: "workflow_run", EntityID: run.ID, RunID: run.ID,
		PayloadJSON: dispatch.PayloadJSON, IdempotencyKey: dispatch.IdempotencyKey, Priority: dispatch.Priority, Actor: run.CreatedBy,
		Reason: "dispatch frozen workflow run after managed inputs persist",
	})
	if err != nil {
		return err
	}
	if created.CommandType != "workflow_run.execute" || created.RunID != run.ID || created.IdempotencyKey != dispatch.IdempotencyKey {
		return fmt.Errorf("created initial workflow run dispatch does not match frozen run")
	}
	return nil
}
