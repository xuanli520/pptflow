package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const standardAuthoringRepairLedgerIdentityDomain = "harbor.standard-authoring.repair-ledger.identity.v1"

// openAuthoringReviewRepairLedger turns a typed immutable review decision into
// one ledger requirement for each producer it invalidates. The decision
// artifact digest is the authorization that a later correction must carry in
// its immutable input manifest before it can close the repair.
func (runtime *FrozenExecutionRuntime) openAuthoringReviewRepairLedger(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, attempt store.StageAttempt, decision store.AuthoringReviewDecision, findingKind store.AuthoringRepairFindingKind) error {
	if findingKind == "" {
		return nil
	}
	if !isCurrentStandardAuthoringRun(run) || !subject.isAuthoringSession() {
		return fmt.Errorf("%w: authoring repair ledger requires a current source/session Run", ErrFrozenExecutionPayload)
	}
	if attempt.RunID != run.ID || attempt.ExecutionStatus != store.StageExecutionCompleted || attempt.Verdict != store.VerdictNeedsRepair || decision.Action != store.ReviewDecisionRequestChanges {
		return fmt.Errorf("%w: authoring review repair ledger does not match a completed request_changes decision", ErrFrozenExecutionPayload)
	}
	contract, err := standardAuthoringContractInputFromSession(ctx, runtime.core.objects, *subject.AuthoringSession)
	if err != nil {
		return fmt.Errorf("load authoring review repair root contract: %w", err)
	}
	evidenceKey, targets, err := authoringReviewRepairPlan(attempt.StageKey, findingKind)
	if err != nil {
		return err
	}
	references, err := runtime.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
	if err != nil {
		return fmt.Errorf("list authoring review decision artifacts: %w", err)
	}
	var evidence *store.ArtifactRef
	for index := range references {
		reference := references[index]
		if reference.ArtifactKey != evidenceKey {
			continue
		}
		if evidence != nil {
			return fmt.Errorf("%w: authoring review decision artifact lineage is ambiguous", ErrFrozenExecutionPayload)
		}
		if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID ||
			reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash {
			return fmt.Errorf("%w: authoring review decision artifact does not match its frozen lineage", ErrFrozenExecutionPayload)
		}
		copy := reference
		evidence = &copy
	}
	if evidence == nil || evidence.ContentDigest == "" {
		return fmt.Errorf("%w: authoring review decision artifact is missing", ErrFrozenExecutionPayload)
	}
	for _, target := range targets {
		entryID, err := standardAuthoringRepairLedgerIdentity(decision.ID, "open:"+string(findingKind)+":"+target)
		if err != nil {
			return err
		}
		if _, err := runtime.core.store.OpenAuthoringRepairLedgerEntry(ctx, store.OpenAuthoringRepairLedgerEntryRequest{
			ID: entryID, RunID: run.ID, ContractDigest: string(contract.ContentDigest), TargetProducer: target,
			FindingKind: findingKind, Reason: decision.Reason, EvidenceDigest: evidence.ContentDigest, Actor: decision.Actor,
		}); err != nil {
			return fmt.Errorf("open authoring review repair ledger: %w", err)
		}
	}
	return nil
}

func authoringReviewRepairPlan(stageKey string, findingKind store.AuthoringRepairFindingKind) (string, []string, error) {
	switch stageKey {
	case workflowadapter.TaskReview:
		switch findingKind {
		case store.AuthoringRepairFindingTaskProposalInvalid:
			return "task_review_decision", []string{workflowadapter.TaskDesign}, nil
		case store.AuthoringRepairFindingSourceAnalysisInvalid:
			return "task_review_decision", []string{workflowadapter.RepoAnalyze}, nil
		}
	case workflowadapter.ContentReview:
		if findingKind == store.AuthoringRepairFindingArtifactInvalid {
			return "content_review_decision", []string{workflowadapter.InstructionGen, workflowadapter.TaskTOMLGen, workflowadapter.DockerfileGen}, nil
		}
	case workflowadapter.SolutionReview:
		if findingKind == store.AuthoringRepairFindingPackageInvalid {
			return "solution_review_decision", standardAuthoringPackageRepairProducers(), nil
		}
	}
	return "", nil, fmt.Errorf("%w: unsupported authoring review repair finding %q for stage %q", ErrFrozenExecutionPayload, findingKind, stageKey)
}

func standardAuthoringPackageRepairProducers() []string {
	return []string{
		workflowadapter.InstructionGen, workflowadapter.TaskTOMLGen, workflowadapter.DockerfileGen,
		workflowadapter.SolveGen, workflowadapter.TestGen, workflowadapter.TestsAnalysis,
	}
}

// openAuthoringPackageAdmissionRepairLedger records deterministic package
// admission failure against each producer whose final bytes must be rebuilt.
func (runtime *FrozenExecutionRuntime) openAuthoringPackageAdmissionRepairLedger(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, attempt store.StageAttempt) error {
	if !subject.isAuthoringSession() || !isCurrentStandardAuthoringRun(run) || attempt.RunID != run.ID ||
		attempt.StageKey != workflowadapter.CodeEdgePackageAdmission || attempt.ExecutionStatus != store.StageExecutionCompleted || attempt.Verdict != store.VerdictNeedsRepair {
		return nil
	}
	contract, err := standardAuthoringContractInputFromSession(ctx, runtime.core.objects, *subject.AuthoringSession)
	if err != nil {
		return fmt.Errorf("load package-admission repair root contract: %w", err)
	}
	references, err := runtime.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
	if err != nil {
		return fmt.Errorf("list package-admission evidence: %w", err)
	}
	var evidence *store.ArtifactRef
	for index := range references {
		if references[index].ArtifactKey != "codeedge_package_admission_report" {
			continue
		}
		if evidence != nil {
			return fmt.Errorf("%w: package-admission report lineage is ambiguous", ErrFrozenExecutionPayload)
		}
		copy := references[index]
		evidence = &copy
	}
	if evidence == nil || evidence.ContentDigest == "" {
		return fmt.Errorf("%w: package-admission report is missing", ErrFrozenExecutionPayload)
	}
	for _, target := range standardAuthoringPackageRepairProducers() {
		entryID, err := standardAuthoringRepairLedgerIdentity(attempt.ID, "open:"+string(store.AuthoringRepairFindingPackageInvalid)+":"+target)
		if err != nil {
			return err
		}
		if _, err := runtime.core.store.OpenAuthoringRepairLedgerEntry(ctx, store.OpenAuthoringRepairLedgerEntryRequest{
			ID: entryID, RunID: run.ID, ContractDigest: string(contract.ContentDigest), TargetProducer: target,
			FindingKind: store.AuthoringRepairFindingPackageInvalid, Reason: "CodeEdge package admission reported a deterministic package violation",
			EvidenceDigest: evidence.ContentDigest, Actor: "system",
		}); err != nil {
			return fmt.Errorf("open package-admission repair ledger: %w", err)
		}
	}
	return nil
}

// resolveAuthoringRepairsForValidatedProducer closes only ledger entries that
// explicitly name this producer. It runs after the Store has committed a
// completed/pass StageAttempt and its immutable artifact refs, then runs again
// on durable-job replay to cover a crash between those commits.
func (runtime *FrozenExecutionRuntime) resolveAuthoringRepairsForValidatedProducer(ctx context.Context, run store.WorkflowRun, subject workflowRunSubject, attempt store.StageAttempt, actor string) error {
	if !subject.isAuthoringSession() || !isCurrentStandardAuthoringRun(run) {
		return nil
	}
	if attempt.RunID != run.ID || attempt.ExecutionStatus != store.StageExecutionCompleted || attempt.Verdict != store.VerdictPass {
		return nil
	}
	entries, err := runtime.core.store.ListOpenAuthoringRepairLedgerEntries(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("list open authoring repair ledger entries: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	contract, err := standardAuthoringContractInputFromSession(ctx, runtime.core.objects, *subject.AuthoringSession)
	if err != nil {
		return fmt.Errorf("load authoring repair root contract: %w", err)
	}
	references, err := runtime.core.store.ListArtifactRefsForAttempt(ctx, attempt.ID)
	if err != nil {
		return fmt.Errorf("list validated producer artifacts: %w", err)
	}
	valid := make([]store.ArtifactRef, 0, len(references))
	for _, reference := range references {
		if reference.RunID != run.ID || reference.StageKey != attempt.StageKey || reference.AttemptID != attempt.ID ||
			reference.SubjectRevisionID != subject.subjectRevisionID() || reference.SubjectDigest != subject.subjectDigest() || reference.WorkflowFingerprint != run.DefinitionHash {
			return fmt.Errorf("%w: validated producer artifact %s does not match its frozen lineage", ErrFrozenExecutionPayload, reference.ID)
		}
		valid = append(valid, reference)
	}
	sort.Slice(valid, func(left, right int) bool {
		if valid[left].ArtifactKey != valid[right].ArtifactKey {
			return valid[left].ArtifactKey < valid[right].ArtifactKey
		}
		return valid[left].ID < valid[right].ID
	})

	for _, entry := range entries {
		if entry.ContractDigest != string(contract.ContentDigest) {
			return fmt.Errorf("%w: authoring repair ledger entry %s has a different root contract", ErrFrozenExecutionPayload, entry.ID)
		}
		if entry.TargetProducer != attempt.StageKey {
			continue
		}
		var artifact *store.ArtifactRef
		for index := range valid {
			explicit, err := authoringArtifactExplicitlySupersedesRepair(valid[index], entry)
			if err != nil {
				return err
			}
			if !explicit {
				continue
			}
			copy := valid[index]
			artifact = &copy
			break
		}
		if artifact == nil {
			// A producer may be retried for another reason. It cannot close this
			// entry unless its frozen inputs explicitly include the ledger evidence.
			continue
		}
		resolutionID, err := standardAuthoringRepairLedgerIdentity(entry.ID, "resolve:"+artifact.ID)
		if err != nil {
			return err
		}
		if _, err := runtime.core.store.ResolveAuthoringRepairLedgerEntry(ctx, store.ResolveAuthoringRepairLedgerEntryRequest{
			ID: resolutionID, RepairID: entry.ID, RunID: run.ID, ContractDigest: entry.ContractDigest,
			Producer: attempt.StageKey, SupersedingArtifactID: artifact.ID, SupersedingAttemptID: attempt.ID,
			Reason: "validated producer artifact explicitly supersedes this repair", Actor: actor,
		}); err != nil {
			return fmt.Errorf("resolve authoring repair ledger entry %s: %w", entry.ID, err)
		}
	}
	return nil
}

func authoringArtifactExplicitlySupersedesRepair(artifact store.ArtifactRef, entry store.AuthoringRepairLedgerEntry) (bool, error) {
	var inputs []workflowkit.ArtifactBinding
	if err := json.Unmarshal([]byte(artifact.InputBindingsJSON), &inputs); err != nil {
		return false, fmt.Errorf("%w: decode validated producer artifact %s input bindings: %v", ErrFrozenExecutionPayload, artifact.ID, err)
	}
	fingerprint, err := workflowkit.FingerprintArtifactBindings(inputs)
	if err != nil {
		return false, fmt.Errorf("%w: validate producer artifact %s input bindings: %v", ErrFrozenExecutionPayload, artifact.ID, err)
	}
	if string(fingerprint) != artifact.InputFingerprint {
		return false, fmt.Errorf("%w: producer artifact %s input fingerprint drift", ErrFrozenExecutionPayload, artifact.ID)
	}
	for _, input := range inputs {
		if string(input.ContentDigest) == entry.EvidenceDigest {
			return true, nil
		}
	}
	return false, nil
}

func standardAuthoringRepairLedgerIdentity(seed, entity string) (string, error) {
	seed = strings.TrimSpace(seed)
	entity = strings.TrimSpace(entity)
	if err := store.ValidateUUIDv7(seed); err != nil {
		return "", fmt.Errorf("authoring repair ledger identity seed: %w", err)
	}
	if entity == "" {
		return "", fmt.Errorf("authoring repair ledger identity entity is required")
	}
	parsed := uuid.MustParse(seed)
	digest := sha256.Sum256([]byte(standardAuthoringRepairLedgerIdentityDomain + "\x00" + entity + "\x00" + parsed.String()))
	derived := parsed
	derived[6] = 0x70 | (digest[0] & 0x0f)
	derived[7] = digest[1]
	derived[8] = 0x80 | (digest[2] & 0x3f)
	copy(derived[9:], digest[3:10])
	return derived.String(), nil
}
