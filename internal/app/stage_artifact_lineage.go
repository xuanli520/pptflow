package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const stageArtifactManifestFormat = "harbor.v2.stage-artifact-manifest.v1"

type stageArtifactCandidate struct {
	attempt store.StageAttempt
	ref     store.ArtifactRef
}

// stageArtifactManifestIndex is the validated, immutable metadata needed to
// turn a durable ArtifactRef back into the object-store reference that owns
// its bytes. ArtifactRef intentionally does not store a size, so accepting a
// digest without this manifest would skip the object store's full integrity
// check.
type stageArtifactManifestIndex struct {
	manifest  store.ArtifactManifest
	payload   stageArtifactManifestPayload
	artifacts map[string]stageArtifactObject
}

// resolveStageInputs reads only immutable artifact lineage from prior durable
// StageAttempts. It refuses references from another revision, workflow, or
// run rather than selecting an arbitrary same-named file from a workspace.
func resolveStageInputs(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, stage workflowkit.StageDescriptor) ([]workflowkit.ArtifactBinding, error) {
	if dataStore == nil {
		return nil, fmt.Errorf("%w: artifact lineage store is required", ErrInvalidStageExecution)
	}
	if objects == nil {
		return nil, fmt.Errorf("%w: artifact object store is required", ErrInvalidStageExecution)
	}
	attempts, err := dataStore.ListStageAttemptsForRun(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	latest := make(map[string]stageArtifactCandidate)
	for _, attempt := range attempts {
		if attempt.ExecutionStatus != store.StageExecutionCompleted || strings.TrimSpace(attempt.ArtifactManifestID) == "" {
			continue
		}
		references, err := dataStore.ListArtifactRefs(ctx, attempt.ArtifactManifestID)
		if err != nil {
			return nil, fmt.Errorf("list artifact refs for stage attempt %s: %w", attempt.ID, err)
		}
		for _, reference := range references {
			if reference.RunID != run.ID || reference.SubjectRevisionID != revision.ID || reference.SubjectDigest != revision.TaskDigest || reference.WorkflowFingerprint != run.DefinitionHash || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID {
				return nil, fmt.Errorf("%w: artifact ref %s does not match frozen run lineage", ErrInvalidStageExecution, reference.ID)
			}
			current, exists := latest[reference.ArtifactKey]
			if !exists || laterArtifactCandidate(attempt, reference, current) {
				latest[reference.ArtifactKey] = stageArtifactCandidate{attempt: attempt, ref: reference}
			}
		}
	}
	bindings := make([]workflowkit.ArtifactBinding, 0, len(stage.Inputs))
	for _, input := range stage.Inputs {
		candidate, found := latest[input.Name]
		if !found {
			if input.Required {
				return nil, fmt.Errorf("%w: stage %q is missing required immutable input %q", ErrInvalidStageExecution, stage.Key, input.Name)
			}
			continue
		}
		if candidate.ref.SchemaVersion != input.SchemaVersion {
			return nil, fmt.Errorf("%w: stage %q input %q has schema %q, want %q", ErrInvalidStageExecution, stage.Key, input.Name, candidate.ref.SchemaVersion, input.SchemaVersion)
		}
		if err := verifyStageArtifactCandidate(ctx, dataStore, objects, run, revision, candidate); err != nil {
			if !input.Required && artifactObjectUnavailable(err) {
				continue
			}
			return nil, fmt.Errorf("%w: stage %q input %q is unavailable: %w", ErrInvalidStageExecution, stage.Key, input.Name, err)
		}
		binding := workflowkit.ArtifactBinding{
			Name:          input.Name,
			ArtifactID:    workflowkit.ArtifactID(candidate.ref.ID),
			ContentDigest: workflowkit.Fingerprint(candidate.ref.ContentDigest),
			SchemaVersion: candidate.ref.SchemaVersion,
		}
		if err := binding.Validate(); err != nil {
			return nil, fmt.Errorf("%w: stage %q input %q: %v", ErrInvalidStageExecution, stage.Key, input.Name, err)
		}
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(left, right int) bool { return bindings[left].Name < bindings[right].Name })
	return bindings, nil
}

func artifactObjectUnavailable(err error) bool {
	return errors.Is(err, workflowruntime.ErrObjectNotFound) ||
		errors.Is(err, workflowruntime.ErrObjectCorrupt) ||
		errors.Is(err, workflowruntime.ErrUnsafeObjectPath)
}

func laterArtifactCandidate(attempt store.StageAttempt, reference store.ArtifactRef, current stageArtifactCandidate) bool {
	if attempt.Ordinal != current.attempt.Ordinal {
		return attempt.Ordinal > current.attempt.Ordinal
	}
	if !attempt.CreatedAt.Equal(current.attempt.CreatedAt) {
		return attempt.CreatedAt.After(current.attempt.CreatedAt)
	}
	if reference.TurnOrdinal != current.ref.TurnOrdinal {
		return reference.TurnOrdinal > current.ref.TurnOrdinal
	}
	return reference.ID > current.ref.ID
}

// stageArtifactManifestPayload is deliberately timestamp-free. A crash after
// publishing objects can replay the same idempotency key and recover the
// existing manifest rather than changing its immutable payload.
type stageArtifactManifestPayload struct {
	Format         string                `json:"format"`
	RunID          string                `json:"run_id"`
	StageAttemptID string                `json:"stage_attempt_id"`
	NodeAttemptID  string                `json:"node_attempt_id"`
	StageKey       workflowkit.StageKey  `json:"stage_key"`
	Artifacts      []stageArtifactObject `json:"artifacts"`
}

type stageArtifactObject struct {
	Key           string                  `json:"key"`
	SchemaVersion string                  `json:"schema_version"`
	Digest        workflowkit.Fingerprint `json:"digest"`
	SizeBytes     int64                   `json:"size_bytes"`
	TurnOrdinal   int                     `json:"turn_ordinal"`
}

func loadStageArtifactManifestIndex(ctx context.Context, dataStore *store.Store, manifestID string) (stageArtifactManifestIndex, error) {
	if dataStore == nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact lineage store is required")
	}
	manifest, err := dataStore.GetArtifactManifest(ctx, manifestID)
	if err != nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("load artifact manifest %s: %w", manifestID, err)
	}
	if manifest == nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s is missing", manifestID)
	}
	fingerprint, err := workflowkit.FingerprintBytes(stageArtifactManifestFormat, []byte(manifest.ManifestJSON))
	if err != nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("fingerprint artifact manifest %s: %w", manifest.ID, err)
	}
	if manifest.ManifestFingerprint != string(fingerprint) {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s fingerprint does not match its immutable payload", manifest.ID)
	}
	var payload stageArtifactManifestPayload
	if err := decodeStrictJSON(manifest.ManifestJSON, &payload); err != nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("decode artifact manifest %s: %w", manifest.ID, err)
	}
	if payload.Format != stageArtifactManifestFormat {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s has unsupported format %q", manifest.ID, payload.Format)
	}
	if strings.TrimSpace(payload.RunID) == "" || strings.TrimSpace(string(payload.StageKey)) == "" {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s has incomplete stage lineage", manifest.ID)
	}
	if err := store.ValidateUUIDv7(payload.StageAttemptID); err != nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s stage attempt ID: %w", manifest.ID, err)
	}
	if err := store.ValidateUUIDv7(payload.NodeAttemptID); err != nil {
		return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s node attempt ID: %w", manifest.ID, err)
	}
	artifacts := make(map[string]stageArtifactObject, len(payload.Artifacts))
	for _, artifact := range payload.Artifacts {
		if strings.TrimSpace(artifact.Key) == "" || strings.TrimSpace(artifact.SchemaVersion) == "" || artifact.TurnOrdinal < 0 {
			return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s has invalid artifact metadata", manifest.ID)
		}
		if err := artifact.Digest.Validate(); err != nil {
			return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s artifact %q digest: %w", manifest.ID, artifact.Key, err)
		}
		if artifact.SizeBytes < 0 {
			return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s artifact %q has negative size", manifest.ID, artifact.Key)
		}
		if _, duplicate := artifacts[artifact.Key]; duplicate {
			return stageArtifactManifestIndex{}, fmt.Errorf("artifact manifest %s has duplicate artifact key %q", manifest.ID, artifact.Key)
		}
		artifacts[artifact.Key] = artifact
	}
	return stageArtifactManifestIndex{manifest: *manifest, payload: payload, artifacts: artifacts}, nil
}

func (index stageArtifactManifestIndex) objectFor(reference store.ArtifactRef) (workflowruntime.ObjectRef, error) {
	if reference.ManifestID != index.manifest.ID {
		return workflowruntime.ObjectRef{}, fmt.Errorf("artifact ref %s does not belong to manifest %s", reference.ID, index.manifest.ID)
	}
	artifact, exists := index.artifacts[reference.ArtifactKey]
	if !exists {
		return workflowruntime.ObjectRef{}, fmt.Errorf("artifact manifest %s does not describe ref %s", index.manifest.ID, reference.ID)
	}
	if artifact.Digest != workflowkit.Fingerprint(reference.ContentDigest) || artifact.SchemaVersion != reference.SchemaVersion || artifact.TurnOrdinal != reference.TurnOrdinal {
		return workflowruntime.ObjectRef{}, fmt.Errorf("artifact manifest %s metadata does not match ref %s", index.manifest.ID, reference.ID)
	}
	return workflowruntime.ObjectRef{Digest: artifact.Digest, SizeBytes: artifact.SizeBytes}, nil
}

func verifyStageArtifactCandidate(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, candidate stageArtifactCandidate) error {
	index, err := loadStageArtifactManifestIndex(ctx, dataStore, candidate.ref.ManifestID)
	if err != nil {
		return err
	}
	return verifyStageArtifactCandidateWithManifest(ctx, objects, index, run, revision, candidate)
}

func verifyStageArtifactCandidateWithManifest(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, index stageArtifactManifestIndex, run store.WorkflowRun, revision store.TaskRevision, candidate stageArtifactCandidate) error {
	if objects == nil {
		return fmt.Errorf("artifact object store is required")
	}
	if index.manifest.SubjectRevisionID != revision.ID || index.manifest.SubjectDigest != revision.TaskDigest || index.manifest.WorkflowFingerprint != run.DefinitionHash ||
		index.payload.RunID != run.ID || index.payload.StageAttemptID != candidate.attempt.ID || string(index.payload.StageKey) != candidate.attempt.StageKey ||
		candidate.ref.AttemptID != candidate.attempt.ID || candidate.ref.RunID != run.ID || candidate.ref.SubjectRevisionID != revision.ID ||
		candidate.ref.SubjectDigest != revision.TaskDigest || candidate.ref.WorkflowFingerprint != run.DefinitionHash || candidate.ref.StageKey != candidate.attempt.StageKey {
		return fmt.Errorf("artifact ref %s does not match immutable stage lineage", candidate.ref.ID)
	}
	object, err := index.objectFor(candidate.ref)
	if err != nil {
		return err
	}
	return VerifyStageArtifactObject(ctx, objects, object)
}

// persistStageArtifacts publishes real executor bytes into the managed object
// store then records immutable manifest/ref lineage. It has no code path that
// creates a synthetic artifact for a completed stage.
func persistStageArtifacts(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, revision store.TaskRevision, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string) (store.ArtifactManifest, []store.ArtifactRef, error) {
	if core == nil || core.store == nil || core.objects == nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("%w: artifact persistence is not configured", ErrInvalidStageExecution)
	}
	if err := RequiredStageArtifacts(stage, artifacts); err != nil {
		return store.ArtifactManifest{}, nil, err
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("%w: fingerprint stage inputs: %v", ErrInvalidStageExecution, err)
	}
	outputs := append([]StageArtifact(nil), artifacts...)
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Key < outputs[right].Key })
	objects := make([]stageArtifactObject, 0, len(outputs))
	for _, output := range outputs {
		object, err := core.objects.PutBytes(ctx, output.Content)
		if err != nil {
			return store.ArtifactManifest{}, nil, fmt.Errorf("publish immutable output %q: %w", output.Key, err)
		}
		objects = append(objects, stageArtifactObject{
			Key: output.Key, SchemaVersion: output.SchemaVersion, Digest: object.Digest, SizeBytes: object.SizeBytes, TurnOrdinal: output.TurnOrdinal,
		})
	}
	payload := stageArtifactManifestPayload{
		Format: stageArtifactManifestFormat, RunID: run.ID, StageAttemptID: stageAttempt.ID, NodeAttemptID: nodeAttempt.ID, StageKey: stage.Key, Artifacts: objects,
	}
	encodedManifest, err := json.Marshal(payload)
	if err != nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("encode stage artifact manifest: %w", err)
	}
	manifestFingerprint, err := workflowkit.FingerprintBytes(stageArtifactManifestFormat, encodedManifest)
	if err != nil {
		return store.ArtifactManifest{}, nil, err
	}
	baseKey := "stage-artifact:" + stageAttempt.ID
	manifest, err := core.store.CreateArtifactManifest(ctx, store.CreateArtifactManifestRequest{
		SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
		ManifestJSON: string(encodedManifest), ManifestFingerprint: string(manifestFingerprint), IdempotencyKey: baseKey + ":manifest",
		Actor: actor, Reason: reason,
	})
	if err != nil {
		return store.ArtifactManifest{}, nil, err
	}
	encodedInputs, err := json.Marshal(inputs)
	if err != nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("encode stage artifact input bindings: %w", err)
	}
	references := make([]store.ArtifactRef, 0, len(objects))
	for _, output := range objects {
		reference, err := core.store.CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
			ManifestID: manifest.ID, ArtifactKey: output.Key, ContentDigest: string(output.Digest), SchemaVersion: output.SchemaVersion,
			RunID: run.ID, StageKey: string(stage.Key), AttemptID: stageAttempt.ID, TurnOrdinal: output.TurnOrdinal,
			SubjectRevisionID: revision.ID, SubjectDigest: revision.TaskDigest, WorkflowFingerprint: run.DefinitionHash,
			InputBindingsJSON: string(encodedInputs), InputFingerprint: string(inputFingerprint), ProducerVersion: stage.Version,
			IdempotencyKey: baseKey + ":artifact:" + output.Key, Actor: actor, Reason: reason,
		})
		if err != nil {
			return store.ArtifactManifest{}, nil, err
		}
		references = append(references, reference)
	}
	return manifest, references, nil
}

// VerifyStageArtifactObject reads the manifest's object reference when a
// caller needs an integrity check. Store refs intentionally retain the digest
// while the object size remains inside the immutable stage manifest payload.
func VerifyStageArtifactObject(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, reference workflowruntime.ObjectRef) error {
	if objects == nil {
		return fmt.Errorf("%w: object store is required", ErrInvalidStageExecution)
	}
	return objects.Verify(ctx, reference)
}
