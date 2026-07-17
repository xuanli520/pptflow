package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const stageArtifactManifestFormat = "harbor.v2.stage-artifact-manifest.v1"

var errStageArtifactStorageUnavailable = errors.New("stage artifact storage is temporarily unavailable")

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
	return resolveStageInputsForSubject(ctx, dataStore, objects, run, taskRevisionSubjectForLineage(run, revision), stage)
}

// resolveStageInputsForSubject is the subject-neutral artifact lineage
// resolver used by the durable runtime.  TaskRevision callers retain the
// small wrapper above; an AuthoringSession uses its source/session binding
// without manufacturing a revision merely to reuse artifact plumbing.
func resolveStageInputsForSubject(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, subject workflowRunSubject, stage workflowkit.StageDescriptor) ([]workflowkit.ArtifactBinding, error) {
	if dataStore == nil {
		return nil, fmt.Errorf("%w: artifact lineage store is required", ErrInvalidStageExecution)
	}
	if objects == nil {
		return nil, fmt.Errorf("%w: artifact object store is required", ErrInvalidStageExecution)
	}
	managed, err := managedRunInputBindingsForStageForSubject(ctx, dataStore, objects, run, subject, stage)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve managed run inputs for stage %q: %w", ErrInvalidStageExecution, stage.Key, err)
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
			if reference.RunID != run.ID || reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID {
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
		if binding, declared := managed[input.Name]; declared {
			if binding.SchemaVersion != input.SchemaVersion {
				return nil, fmt.Errorf("%w: stage %q managed input %q has schema %q, want %q", ErrInvalidStageExecution, stage.Key, input.Name, binding.SchemaVersion, input.SchemaVersion)
			}
			bindings = append(bindings, binding)
			continue
		}
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
		if err := verifyStageArtifactCandidateForSubject(ctx, dataStore, objects, run, subject, candidate); err != nil {
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

func taskRevisionSubjectForLineage(run store.WorkflowRun, revision store.TaskRevision) workflowRunSubject {
	subjectID := run.SubjectID
	if subjectID == "" {
		subjectID = run.TaskID
	}
	return workflowRunSubject{
		Binding:  workflowkit.SubjectBinding{SubjectID: subjectID, RevisionID: revision.ID, Digest: workflowkit.SubjectDigest(revision.TaskDigest)},
		Kind:     store.WorkflowRunSubjectTaskRevision,
		Revision: &revision,
	}
}

// managedRunInputBindingsForStage resolves only manifest-declared run inputs
// from the final frozen spec. It runs before StageAttempt lineage selection so
// a later same-named stage artifact can never shadow an intrinsic input.
func managedRunInputBindingsForStage(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, stage workflowkit.StageDescriptor) (map[string]workflowkit.ArtifactBinding, error) {
	return managedRunInputBindingsForStageForSubject(ctx, dataStore, objects, run, taskRevisionSubjectForLineage(run, revision), stage)
}

func managedRunInputBindingsForStageForSubject(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, subject workflowRunSubject, stage workflowkit.StageDescriptor) (map[string]workflowkit.ArtifactBinding, error) {
	if subject.isAuthoringSession() {
		var manifest runManifest
		if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
			return nil, fmt.Errorf("decode authoring Run manifest: %w", err)
		}
		if manifest.Inputs == nil || len(manifest.Inputs.ManagedInputs) != 0 {
			return nil, fmt.Errorf("authoring Run declares unsupported task-revision managed inputs")
		}
		specification, _, _, err := canonicalFrozenRunExecutionSpec(manifest, run)
		if err != nil {
			return nil, err
		}
		if err := validateCurrentStandardAuthoringFrozenContract(run, manifest, specification); err != nil {
			return nil, err
		}
		environmentPolicy, err := standardAuthoringEnvironmentPolicyInputFromSession(*subject.AuthoringSession)
		if err != nil {
			return nil, err
		}
		if err := validateStandardAuthoringEnvironmentPolicyBindings(specification, environmentPolicy); err != nil {
			return nil, err
		}
		result := make(map[string]workflowkit.ArtifactBinding)
		for _, input := range stage.Inputs {
			if input.Name != workflowadapter.StandardAuthoringEnvironmentPolicyArtifact {
				continue
			}
			binding := environmentPolicy.artifactBinding()
			if input.SchemaVersion != binding.SchemaVersion {
				return nil, fmt.Errorf("authoring stage %q environment policy schema %q, want %q", stage.Key, input.SchemaVersion, binding.SchemaVersion)
			}
			result[binding.Name] = binding
		}
		return result, nil
	}
	if !subject.isTaskRevision() || subject.Revision == nil {
		return nil, fmt.Errorf("artifact lineage has an unsupported workflow subject")
	}
	revision := *subject.Revision
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("decode run manifest: %w", err)
	}
	if manifest.Inputs == nil || len(manifest.Inputs.ManagedInputs) == 0 {
		return nil, nil
	}
	if manifest.Format != "harbor.workflow-run-manifest.v2" || manifest.RunID != run.ID || manifest.TaskID != run.TaskID || manifest.Revision != run.RevisionID {
		return nil, fmt.Errorf("run manifest does not match workflow run")
	}
	specification, _, _, err := canonicalFrozenRunExecutionSpec(manifest, run)
	if err != nil {
		return nil, err
	}
	core := &lifecycleServiceCore{store: dataStore, objects: objects}
	if err := verifyManagedRunInputs(ctx, core, run, revision, manifest, specification); err != nil {
		return nil, err
	}
	resolution, err := specification.ResolveStageOperation(stage.Key)
	if err != nil {
		return nil, err
	}
	artifactByID := make(map[workflowkit.ArtifactID]workflowadapter.ArtifactReference, len(specification.References.Artifacts))
	for _, artifact := range specification.References.Artifacts {
		artifactByID[artifact.ID] = artifact
	}
	bindingByPort := make(map[string]workflowkit.ArtifactID, len(resolution.ArtifactInputs))
	for _, binding := range resolution.ArtifactInputs {
		if _, duplicate := bindingByPort[binding.Port]; duplicate {
			return nil, fmt.Errorf("frozen stage binding has duplicate artifact input port %q", binding.Port)
		}
		bindingByPort[binding.Port] = binding.ArtifactID
	}
	stageInputs := make(map[string]workflowkit.ArtifactSpec, len(stage.Inputs))
	for _, input := range stage.Inputs {
		stageInputs[input.Name] = input
	}
	result := make(map[string]workflowkit.ArtifactBinding, len(manifest.Inputs.ManagedInputs))
	for _, input := range manifest.Inputs.ManagedInputs {
		stageInput, consumes := stageInputs[input.Port]
		if !consumes {
			continue
		}
		artifactID, bound := bindingByPort[input.Port]
		if !bound || artifactID != workflowkit.ArtifactID(input.ID) {
			return nil, fmt.Errorf("frozen stage binding does not use managed input %q", input.Port)
		}
		artifact, present := artifactByID[artifactID]
		if !present || artifact.ContentDigest != input.ContentDigest || artifact.SchemaVersion != input.SchemaVersion || stageInput.SchemaVersion != input.SchemaVersion {
			return nil, fmt.Errorf("managed input %q does not match frozen stage artifact contract", input.Port)
		}
		binding := workflowkit.ArtifactBinding{Name: input.Port, ArtifactID: artifactID, ContentDigest: input.ContentDigest, SchemaVersion: input.SchemaVersion}
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		result[input.Port] = binding
	}
	return result, nil
}

// newStageInputReader gives a V2 executor read-only access to the exact
// bindings verified for its current stage. It never resolves a path or accepts
// a same-named artifact from a later attempt, so a plugin cannot bypass frozen
// lineage by opening the object store directly.
func newStageInputReader(dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, bindings []workflowkit.ArtifactBinding) func(context.Context, workflowkit.ArtifactBinding) ([]byte, error) {
	return newStageInputReaderForSubject(dataStore, objects, run, taskRevisionSubjectForLineage(run, revision), bindings)
}

func newStageInputReaderForSubject(dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, subject workflowRunSubject, bindings []workflowkit.ArtifactBinding) func(context.Context, workflowkit.ArtifactBinding) ([]byte, error) {
	allowed := make(map[workflowkit.ArtifactID]workflowkit.ArtifactBinding, len(bindings))
	for _, binding := range bindings {
		allowed[binding.ArtifactID] = binding
	}
	return func(ctx context.Context, requested workflowkit.ArtifactBinding) ([]byte, error) {
		if err := requested.Validate(); err != nil {
			return nil, fmt.Errorf("%w: requested input binding: %v", ErrInvalidStageExecution, err)
		}
		expected, found := allowed[requested.ArtifactID]
		if !found || expected != requested {
			return nil, fmt.Errorf("%w: requested input is not part of the frozen stage bindings", ErrInvalidStageExecution)
		}
		if dataStore == nil || objects == nil {
			return nil, fmt.Errorf("%w: stage input reader is not configured", ErrInvalidStageExecution)
		}
		if subject.isAuthoringSession() {
			if !isCurrentStandardAuthoringRun(run) {
				return nil, fmt.Errorf("%w: authoring Run is not bound to the current Standard authoring template", ErrInvalidStageExecution)
			}
			environmentPolicy, err := standardAuthoringEnvironmentPolicyInputFromSession(*subject.AuthoringSession)
			if err != nil {
				return nil, fmt.Errorf("%w: authoring session environment policy: %v", ErrInvalidStageExecution, err)
			}
			if requested == environmentPolicy.artifactBinding() {
				return append([]byte(nil), environmentPolicy.CanonicalJSON...), nil
			}
		}
		if subject.isTaskRevision() {
			managed, managedErr := readManagedRunInput(ctx, dataStore, objects, run, *subject.Revision, requested)
			if managedErr != nil {
				return nil, fmt.Errorf("%w: managed run input: %w", ErrInvalidStageExecution, managedErr)
			}
			if managed != nil {
				return managed, nil
			}
		}
		reference, err := dataStore.GetArtifactRef(ctx, string(requested.ArtifactID))
		if err != nil {
			return nil, err
		}
		if reference == nil || reference.ContentDigest != string(requested.ContentDigest) || reference.SchemaVersion != requested.SchemaVersion || reference.ArtifactKey != requested.Name ||
			reference.RunID != run.ID || reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash {
			return nil, fmt.Errorf("%w: requested input no longer matches its frozen lineage", ErrInvalidStageExecution)
		}
		index, err := loadStageArtifactManifestIndex(ctx, dataStore, reference.ManifestID)
		if err != nil {
			return nil, err
		}
		if index.manifest.SubjectRevisionID != subject.subjectRevisionID() || index.manifest.SubjectDigest != subject.subjectDigest() || index.manifest.WorkflowFingerprint != run.DefinitionHash || index.payload.RunID != run.ID ||
			index.payload.StageAttemptID != reference.AttemptID || string(index.payload.StageKey) != reference.StageKey {
			return nil, fmt.Errorf("%w: requested input manifest does not match frozen lineage", ErrInvalidStageExecution)
		}
		object, err := index.objectFor(*reference)
		if err != nil {
			return nil, err
		}
		return objects.ReadAll(ctx, object)
	}
}

// readManagedRunInput reads a manifest-declared initial input only when the
// requested binding exactly names its ID, port, digest, and schema. A durable
// input record that is not declared by this Run's frozen manifest is never an
// execution capability.
func readManagedRunInput(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, revision store.TaskRevision, requested workflowkit.ArtifactBinding) ([]byte, error) {
	var manifest runManifest
	if err := decodeStrictJSON(run.RunManifestJSON, &manifest); err != nil {
		return nil, fmt.Errorf("decode run manifest: %w", err)
	}
	if manifest.Inputs == nil || len(manifest.Inputs.ManagedInputs) == 0 {
		return nil, nil
	}
	for _, input := range manifest.Inputs.ManagedInputs {
		if input.ID != string(requested.ArtifactID) {
			continue
		}
		if input.Port != requested.Name || input.ContentDigest != requested.ContentDigest || input.SchemaVersion != requested.SchemaVersion || input.RevisionDigest != workflowkit.SubjectDigest(revision.TaskDigest) {
			return nil, fmt.Errorf("requested binding does not match frozen managed run input")
		}
		persisted, err := dataStore.GetRunInputArtifact(ctx, input.ID)
		if err != nil {
			return nil, err
		}
		if persisted == nil || persisted.RunID != run.ID || persisted.TaskID != run.TaskID || persisted.RevisionID != revision.ID ||
			persisted.RevisionDigest != revision.TaskDigest || persisted.Port != input.Port || persisted.ContentDigest != string(input.ContentDigest) ||
			persisted.SchemaVersion != input.SchemaVersion || persisted.SizeBytes != input.SizeBytes {
			return nil, fmt.Errorf("durable managed run input does not match frozen lineage")
		}
		return objects.ReadAll(ctx, input.objectRef())
	}
	return nil, nil
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
		if !errors.Is(err, store.ErrInvalidUUIDv7Identity) && !errors.Is(err, store.ErrInvalidJobFailure) {
			return stageArtifactManifestIndex{}, fmt.Errorf("%w: load artifact manifest %s: %v", errStageArtifactStorageUnavailable, manifestID, err)
		}
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
	return verifyStageArtifactCandidateForSubject(ctx, dataStore, objects, run, taskRevisionSubjectForLineage(run, revision), candidate)
}

func verifyStageArtifactCandidateForSubject(ctx context.Context, dataStore *store.Store, objects *workflowruntime.ArtifactObjectStore, run store.WorkflowRun, subject workflowRunSubject, candidate stageArtifactCandidate) error {
	index, err := loadStageArtifactManifestIndex(ctx, dataStore, candidate.ref.ManifestID)
	if err != nil {
		return err
	}
	return verifyStageArtifactCandidateWithManifestForSubject(ctx, objects, index, run, subject, candidate)
}

func verifyStageArtifactCandidateWithManifest(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, index stageArtifactManifestIndex, run store.WorkflowRun, revision store.TaskRevision, candidate stageArtifactCandidate) error {
	return verifyStageArtifactCandidateWithManifestForSubject(ctx, objects, index, run, taskRevisionSubjectForLineage(run, revision), candidate)
}

func verifyStageArtifactCandidateWithManifestForSubject(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, index stageArtifactManifestIndex, run store.WorkflowRun, subject workflowRunSubject, candidate stageArtifactCandidate) error {
	if objects == nil {
		return fmt.Errorf("artifact object store is required")
	}
	if index.manifest.SubjectRevisionID != subject.subjectRevisionID() || index.manifest.SubjectDigest != subject.subjectDigest() || index.manifest.WorkflowFingerprint != run.DefinitionHash ||
		index.payload.RunID != run.ID || index.payload.StageAttemptID != candidate.attempt.ID || string(index.payload.StageKey) != candidate.attempt.StageKey ||
		candidate.ref.AttemptID != candidate.attempt.ID || candidate.ref.RunID != run.ID || candidate.ref.SubjectRevisionID != subject.subjectRevisionID() ||
		candidate.ref.SubjectDigest != subject.subjectDigest() || candidate.ref.WorkflowFingerprint != run.DefinitionHash || candidate.ref.StageKey != candidate.attempt.StageKey {
		return fmt.Errorf("artifact ref %s does not match immutable stage lineage", candidate.ref.ID)
	}
	object, err := index.objectFor(candidate.ref)
	if err != nil {
		return err
	}
	if err := VerifyStageArtifactObject(ctx, objects, object); err != nil {
		if !artifactObjectUnavailable(err) {
			return fmt.Errorf("%w: verify artifact ref %s: %v", errStageArtifactStorageUnavailable, candidate.ref.ID, err)
		}
		return err
	}
	return nil
}

// persistStageArtifacts publishes real executor bytes into the managed object
// store then records immutable manifest/ref lineage. It has no code path that
// creates a synthetic artifact for a completed stage.
func persistStageArtifacts(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, revision store.TaskRevision, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string) (store.ArtifactManifest, []store.ArtifactRef, error) {
	return persistStageArtifactsForSubject(ctx, core, run, taskRevisionSubjectForLineage(run, revision), stageAttempt, nodeAttempt, stage, inputs, artifacts, actor, reason)
}

func persistStageArtifactsForSubject(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, subject workflowRunSubject, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string) (store.ArtifactManifest, []store.ArtifactRef, error) {
	return persistStageArtifactsWithCompletenessForSubject(ctx, core, run, subject, stageAttempt, nodeAttempt, stage, inputs, artifacts, actor, reason, true)
}

// persistStageEvidence records declared partial artifacts produced by a failed
// or interrupted execution. These artifacts are forensic evidence only:
// resolveStageInputs deliberately considers manifests from completed attempts
// exclusively, so diagnostic bytes can never become downstream workflow input.
func persistStageEvidence(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, revision store.TaskRevision, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string) (store.ArtifactManifest, []store.ArtifactRef, error) {
	return persistStageEvidenceForSubject(ctx, core, run, taskRevisionSubjectForLineage(run, revision), stageAttempt, nodeAttempt, stage, inputs, artifacts, actor, reason)
}

func persistStageEvidenceForSubject(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, subject workflowRunSubject, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string) (store.ArtifactManifest, []store.ArtifactRef, error) {
	return persistStageArtifactsWithCompletenessForSubject(ctx, core, run, subject, stageAttempt, nodeAttempt, stage, inputs, artifacts, actor, reason, false)
}

func persistStageArtifactsWithCompleteness(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, revision store.TaskRevision, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string, requireComplete bool) (store.ArtifactManifest, []store.ArtifactRef, error) {
	return persistStageArtifactsWithCompletenessForSubject(ctx, core, run, taskRevisionSubjectForLineage(run, revision), stageAttempt, nodeAttempt, stage, inputs, artifacts, actor, reason, requireComplete)
}

func persistStageArtifactsWithCompletenessForSubject(ctx context.Context, core *lifecycleServiceCore, run store.WorkflowRun, subject workflowRunSubject, stageAttempt store.StageAttempt, nodeAttempt store.NodeAttempt, stage workflowkit.StageDescriptor, inputs []workflowkit.ArtifactBinding, artifacts []StageArtifact, actor, reason string, requireComplete bool) (store.ArtifactManifest, []store.ArtifactRef, error) {
	if core == nil || core.store == nil || core.objects == nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("%w: artifact persistence is not configured", ErrInvalidStageExecution)
	}
	if !subject.isTaskRevision() && !subject.isAuthoringSession() {
		return store.ArtifactManifest{}, nil, fmt.Errorf("%w: artifact persistence has an unsupported workflow subject", ErrInvalidStageExecution)
	}
	if err := validateStageArtifactsForPersistence(stage, artifacts, requireComplete); err != nil {
		return store.ArtifactManifest{}, nil, err
	}
	inputFingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return store.ArtifactManifest{}, nil, fmt.Errorf("%w: fingerprint stage inputs: %v", ErrInvalidStageExecution, err)
	}
	outputs := append([]StageArtifact(nil), artifacts...)
	sort.Slice(outputs, func(left, right int) bool { return outputs[left].Key < outputs[right].Key })
	reservedIDs := make(map[string]struct{}, len(outputs))
	objects := make([]stageArtifactObject, 0, len(outputs))
	for _, output := range outputs {
		if output.ID != "" {
			if _, duplicate := reservedIDs[output.ID]; duplicate {
				return store.ArtifactManifest{}, nil, fmt.Errorf("%w: stage %q returned duplicate reserved artifact ID %s", ErrInvalidStageExecution, stage.Key, output.ID)
			}
			reservedIDs[output.ID] = struct{}{}
		}
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
		SubjectRevisionID: subject.subjectRevisionID(), SubjectDigest: subject.subjectDigest(), WorkflowFingerprint: run.DefinitionHash,
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
		reservedID := ""
		for _, artifact := range outputs {
			if artifact.Key == output.Key {
				reservedID = artifact.ID
				break
			}
		}
		reference, err := core.store.CreateArtifactRef(ctx, store.CreateArtifactRefRequest{
			ID: reservedID, ManifestID: manifest.ID, ArtifactKey: output.Key, ContentDigest: string(output.Digest), SchemaVersion: output.SchemaVersion,
			RunID: run.ID, StageKey: string(stage.Key), AttemptID: stageAttempt.ID, TurnOrdinal: output.TurnOrdinal,
			SubjectRevisionID: subject.subjectRevisionID(), SubjectDigest: subject.subjectDigest(), WorkflowFingerprint: run.DefinitionHash,
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

func validateStageArtifactsForPersistence(stage workflowkit.StageDescriptor, artifacts []StageArtifact, requireComplete bool) error {
	if requireComplete {
		return RequiredStageArtifacts(stage, artifacts)
	}
	declared := make(map[string]workflowkit.ArtifactSpec, len(stage.Outputs))
	for _, output := range stage.Outputs {
		declared[output.Name] = output
	}
	seen := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		key := strings.TrimSpace(artifact.Key)
		if key == "" {
			return fmt.Errorf("%w: diagnostic artifact key is required", ErrInvalidStageExecution)
		}
		output, found := declared[key]
		if !found {
			return fmt.Errorf("%w: stage %q returned undeclared diagnostic artifact %q", ErrInvalidStageExecution, stage.Key, key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: stage %q returned duplicate diagnostic artifact %q", ErrInvalidStageExecution, stage.Key, key)
		}
		if artifact.SchemaVersion != output.SchemaVersion || artifact.TurnOrdinal < 0 {
			return fmt.Errorf("%w: stage %q diagnostic artifact %q does not match its frozen output contract", ErrInvalidStageExecution, stage.Key, key)
		}
		seen[key] = struct{}{}
	}
	return nil
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
