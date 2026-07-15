package workflowkit

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExecutionBudgetValidatesHierarchyAndNestedEnvelope(t *testing.T) {
	budget := testBudget()
	if err := budget.Validate(); err != nil {
		t.Fatalf("validate budget: %v", err)
	}

	tooSmallAttempt := budget
	tooSmallAttempt.AttemptTimeout = 20 * time.Second
	if err := tooSmallAttempt.Validate(); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("small attempt error = %v, want ErrInvalidBudget", err)
	}

	tooSmallElapsed := budget
	tooSmallElapsed.MaxElapsed = 44 * time.Second
	if err := tooSmallElapsed.Validate(); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("small elapsed error = %v, want ErrInvalidBudget", err)
	}

	if err := ValidateNestedBudget(51*time.Second, budget); err != nil {
		t.Fatalf("nested budget should fit: %v", err)
	}
	if err := ValidateNestedBudget(44*time.Second, budget); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nested overflow error = %v, want ErrInvalidBudget", err)
	}
}

func TestWorkflowDescriptorFingerprintIsCanonicalAndDAGValidated(t *testing.T) {
	workflow := testWorkflow(t)
	fingerprint, err := workflow.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint workflow: %v", err)
	}

	reordered := workflow.Clone()
	reordered.Stages[0], reordered.Stages[2] = reordered.Stages[2], reordered.Stages[0]
	for index := range reordered.Stages {
		stage := &reordered.Stages[index]
		stage.ReadSet = append([]ResourceKey(nil), reverseResources(stage.ReadSet)...)
		stage.WriteSet = append([]ResourceKey(nil), reverseResources(stage.WriteSet)...)
		stage.Capabilities = append(CapabilitySet(nil), reverseCapabilities(stage.Capabilities)...)
	}
	other, err := reordered.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint reordered workflow: %v", err)
	}
	if other != fingerprint {
		t.Fatalf("canonical fingerprints differ: %s != %s", other, fingerprint)
	}

	cycle := workflow.Clone()
	cycle.Stages[0].Dependencies = []StageKey{"deliver"}
	if err := cycle.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("cycle error = %v, want ErrInvalidDescriptor", err)
	}
	unsafeRetry := workflow.Clone()
	unsafeRetry.Stages[0].Retry = RetryPolicy{Retryable: []FailureClass{FailureUnknown}}
	if err := unsafeRetry.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("unsafe retry error = %v, want ErrInvalidDescriptor", err)
	}
}

func TestExecutionStateAndOutcomeSeparateMechanicsFromVerdict(t *testing.T) {
	if !CanTransitionExecution(StatusRunning, StatusCompleted) {
		t.Fatal("running -> completed should be legal")
	}
	if CanTransitionExecution(StatusCompleted, StatusRunning) {
		t.Fatal("terminal completed state must not reopen")
	}
	if !CanTransitionExecution(StatusInDoubt, StatusReconciling) {
		t.Fatal("in_doubt -> reconciling should be legal")
	}

	repairable := Outcome{Status: StatusCompleted, Verdict: VerdictNeedsRepair}
	if err := repairable.Validate(); err != nil {
		t.Fatalf("completed repairable outcome should be valid: %v", err)
	}
	infra := Outcome{Status: StatusInfraFailed, Failure: FailureTimeout}
	if err := infra.Validate(); err != nil {
		t.Fatalf("infra failure should be valid: %v", err)
	}
	invalid := Outcome{Status: StatusInfraFailed, Verdict: VerdictNeedsRepair, Failure: FailureTimeout}
	if err := invalid.Validate(); !errors.Is(err, ErrInvalidAttemptRecord) {
		t.Fatalf("mixed outcome error = %v, want ErrInvalidAttemptRecord", err)
	}
}

func TestAttemptLogIsAppendOnlyAndRetainsFailureArtifacts(t *testing.T) {
	workflow := testWorkflow(t)
	artifact := testArtifact(t, workflow, "attempt-1")
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	identity := AttemptIdentity{ID: "attempt-1", Kind: AttemptStage, ScopeID: "source", Ordinal: 1}
	opened, err := NewOpenedAttemptRecord("record-1", 1, identity, StatusRunning, now)
	if err != nil {
		t.Fatalf("open attempt: %v", err)
	}
	checkpoint, err := NewCheckpointAttemptRecord("record-2", 2, identity, StatusRunning, []ArtifactRef{artifact}, now.Add(time.Second))
	if err != nil {
		t.Fatalf("checkpoint attempt: %v", err)
	}
	terminal, err := NewTerminalAttemptRecord("record-3", 3, identity, Outcome{Status: StatusInfraFailed, Failure: FailureTimeout}, []ArtifactRef{artifact}, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("complete attempt: %v", err)
	}

	log, err := NewAttemptLog(opened, checkpoint, terminal)
	if err != nil {
		t.Fatalf("append records: %v", err)
	}
	snapshot, ok := log.Snapshot("attempt-1")
	if !ok {
		t.Fatal("attempt snapshot missing")
	}
	if snapshot.Status != StatusInfraFailed || snapshot.Outcome == nil || snapshot.Outcome.Failure != FailureTimeout {
		t.Fatalf("snapshot = %#v, want terminal timeout", snapshot)
	}
	if len(snapshot.Artifacts) != 1 || snapshot.Artifacts[0].ID != artifact.ID {
		t.Fatalf("failure artifact was not retained: %#v", snapshot.Artifacts)
	}

	records := log.Records()
	records[1].Artifacts[0].ProducerVersion = "mutated"
	again, _ := log.Snapshot("attempt-1")
	if again.Artifacts[0].ProducerVersion == "mutated" {
		t.Fatal("records leaked mutable artifact state into log")
	}

	if _, err := log.Append(terminal); !errors.Is(err, ErrInvalidAttemptRecord) {
		t.Fatalf("second terminal record error = %v, want ErrInvalidAttemptRecord", err)
	}
	retryIdentity := AttemptIdentity{ID: "attempt-2", Kind: AttemptStage, ScopeID: "source", RetryOfAttemptID: "attempt-1", Ordinal: 2}
	retry, err := NewOpenedAttemptRecord("record-4", 4, retryIdentity, StatusRunning, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("open retry: %v", err)
	}
	if _, err := log.Append(retry); err != nil {
		t.Fatalf("append retry: %v", err)
	}

	badFirstIdentity := AttemptIdentity{ID: "bad", Kind: AttemptStage, ScopeID: "source", Ordinal: 2}
	badFirst, err := NewOpenedAttemptRecord("bad-record", 1, badFirstIdentity, StatusRunning, now)
	if err != nil {
		t.Fatalf("make bad first record: %v", err)
	}
	if _, err := NewAttemptLog(badFirst); !errors.Is(err, ErrInvalidAttemptRecord) {
		t.Fatalf("non-first ordinal error = %v, want ErrInvalidAttemptRecord", err)
	}
}

func TestArtifactFingerprintAndLineageValidation(t *testing.T) {
	first := ArtifactBinding{Name: "report", ArtifactID: "artifact-1", ContentDigest: SHA256Fingerprint([]byte("one")), SchemaVersion: "v1"}
	second := ArtifactBinding{Name: "config", ArtifactID: "artifact-2", ContentDigest: SHA256Fingerprint([]byte("two")), SchemaVersion: "v1"}
	forward, err := FingerprintArtifactBindings([]ArtifactBinding{first, second})
	if err != nil {
		t.Fatalf("fingerprint bindings: %v", err)
	}
	reversed, err := FingerprintArtifactBindings([]ArtifactBinding{second, first})
	if err != nil {
		t.Fatalf("fingerprint reversed bindings: %v", err)
	}
	if forward != reversed {
		t.Fatalf("input fingerprint changed with binding order: %s != %s", forward, reversed)
	}
	changed := second
	changed.ContentDigest = SHA256Fingerprint([]byte("changed"))
	different, err := FingerprintArtifactBindings([]ArtifactBinding{first, changed})
	if err != nil {
		t.Fatalf("fingerprint changed bindings: %v", err)
	}
	if different == forward {
		t.Fatal("input fingerprint did not change with content digest")
	}

	workflow := testWorkflow(t)
	artifact := testArtifact(t, workflow, "attempt-1")
	if err := artifact.Validate(); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
	artifact.InputFingerprint = SHA256Fingerprint([]byte("wrong"))
	if err := artifact.Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("lineage mismatch error = %v, want ErrInvalidArtifact", err)
	}
}

func TestSubjectDigestAllowsVersionedDomainIdentityWithoutRelaxingObjectFingerprint(t *testing.T) {
	versioned := SubjectDigest("subject.v2:sha256:" + strings.Repeat("a", 64))
	if err := versioned.Validate(); err != nil {
		t.Fatalf("versioned subject digest rejected: %v", err)
	}
	if err := Fingerprint(versioned).Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("versioned subject digest was accepted as an object fingerprint: %v", err)
	}
	if err := SubjectDigest("subject.V2:sha256:" + strings.Repeat("a", 64)).Validate(); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("noncanonical subject digest namespace error = %v, want ErrInvalidArtifact", err)
	}
}

func TestContinuationPlanFreezesCoverageTopologyAndExternalConfirmation(t *testing.T) {
	workflow := testWorkflow(t)
	workflowFingerprint := mustWorkflowFingerprint(t, workflow)
	emptyInputs := mustInputFingerprint(t, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	snapshot := ContinuationPlanSnapshot{
		PlanID:             "plan-1",
		CommandID:          "command-1",
		Strategy:           StrategyRecompute,
		BaseCheckpoint:     testCheckpoint(workflowFingerprint),
		NextExecutionEpoch: 1,
		SourceRunID:        "run-1",
		TargetRunRelation:  RelationSameRunAttempt,
		SubjectRevisionID:  "revision-1",
		SubjectDigest:      SubjectDigest(SHA256Fingerprint([]byte("subject-1"))),
		Nodes: []NodeTransition{
			{NodeID: "source", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"requested"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "verify", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"dependency"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "deliver", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"requested"}, ExpectedInputFingerprint: emptyInputs},
		},
		Schedule: []ScheduleBatch{
			{ID: "batch-source", NodeIDs: []NodeID{"source"}},
			{ID: "batch-verify", NodeIDs: []NodeID{"verify"}},
			{ID: "batch-deliver", NodeIDs: []NodeID{"deliver"}},
		},
		ExternalEffectConfirmations: []ExternalEffectConfirmation{{NodeID: "deliver", IdempotencyKey: "delivery-1", Actor: "local-user", ConfirmedAt: now}},
		ExpiresAt:                   now.Add(time.Hour),
	}
	plan, err := FreezeContinuationPlan(snapshot, workflow)
	if err != nil {
		t.Fatalf("freeze valid plan: %v", err)
	}
	if err := plan.Validate(workflow); err != nil {
		t.Fatalf("validate frozen plan: %v", err)
	}
	if plan.ID() != "plan-1" || plan.Fingerprint() == "" || !plan.IsExpired(now.Add(2*time.Hour)) {
		t.Fatalf("frozen plan accessors returned inconsistent values")
	}
	mutated := plan.Snapshot()
	mutated.Nodes[0].ReasonCodes[0] = "mutated"
	if plan.Snapshot().Nodes[0].ReasonCodes[0] == "mutated" {
		t.Fatal("plan snapshot mutation changed frozen plan")
	}

	withoutConfirmation := snapshot.Clone()
	withoutConfirmation.ExternalEffectConfirmations = nil
	if _, err := FreezeContinuationPlan(withoutConfirmation, workflow); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("missing external confirmation error = %v, want ErrInvalidContinuationPlan", err)
	}

	badTopology := snapshot.Clone()
	badTopology.Schedule = []ScheduleBatch{
		{ID: "batch-1", NodeIDs: []NodeID{"source", "verify"}},
		{ID: "batch-2", NodeIDs: []NodeID{"deliver"}},
	}
	if _, err := FreezeContinuationPlan(badTopology, workflow); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("same-batch dependency error = %v, want ErrInvalidContinuationPlan", err)
	}

	undeclaredInput := snapshot.Clone()
	binding := ArtifactBinding{Name: "undeclared", ArtifactID: "artifact-undeclared", ContentDigest: SHA256Fingerprint([]byte("input")), SchemaVersion: "v1"}
	undeclaredInput.Nodes[0].InputBindings = []ArtifactBinding{binding}
	undeclaredInput.Nodes[0].ExpectedInputFingerprint = mustInputFingerprint(t, undeclaredInput.Nodes[0].InputBindings)
	if _, err := FreezeContinuationPlan(undeclaredInput, workflow); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("undeclared input binding error = %v, want ErrInvalidContinuationPlan", err)
	}
}

func TestRevisionContinuationPlanKeepsDomainCandidateStorageOutOfPublicSnapshot(t *testing.T) {
	workflow := testWorkflow(t)
	workflowFingerprint := mustWorkflowFingerprint(t, workflow)
	emptyInputs := mustInputFingerprint(t, nil)
	now := time.Date(2026, time.July, 15, 10, 0, 0, 0, time.UTC)
	snapshot := ContinuationPlanSnapshot{
		PlanID:             "revision-plan",
		CommandID:          "revision-command",
		Strategy:           StrategyReviseSubject,
		BaseCheckpoint:     testCheckpoint(workflowFingerprint),
		NextExecutionEpoch: 1,
		SourceRunID:        "source-run",
		TargetRunRelation:  RelationChildRun,
		PreparedChangeID:   "prepared-change",
		SubjectRevisionID:  "replacement-revision",
		SubjectDigest:      SubjectDigest(SHA256Fingerprint([]byte("replacement-subject"))),
		Nodes: []NodeTransition{
			{NodeID: "source", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"subject_changed"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "verify", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"subject_changed"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "deliver", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"subject_changed"}, ExpectedInputFingerprint: emptyInputs},
		},
		Schedule: []ScheduleBatch{
			{ID: "source", NodeIDs: []NodeID{"source"}},
			{ID: "verify", NodeIDs: []NodeID{"verify"}},
			{ID: "deliver", NodeIDs: []NodeID{"deliver"}},
		},
		ExternalEffectConfirmations: []ExternalEffectConfirmation{{NodeID: "deliver", IdempotencyKey: "delivery", Actor: "operator", ConfirmedAt: now}},
		ExpiresAt:                   now.Add(time.Hour),
	}
	plan, err := FreezeContinuationPlan(snapshot, workflow)
	if err != nil {
		t.Fatalf("freeze generic revised-subject plan: %v", err)
	}
	encoded, err := json.Marshal(plan.Snapshot())
	if err != nil {
		t.Fatalf("marshal generic revised-subject plan: %v", err)
	}
	if strings.Contains(string(encoded), "candidate_id") || strings.Contains(string(encoded), "revision_candidate") {
		t.Fatalf("public generic continuation leaked domain candidate storage: %s", encoded)
	}
}

func TestContinuationPlanKeepsOperatorOnlyStagesOutOfWorkerSchedules(t *testing.T) {
	workflow := testWorkflow(t)
	workflow.Stages[2].Dispatch = StageDispatchOperatorOnly
	workflow.Stages[2].Retry = RetryPolicy{}
	workflow.Stages[2].Reuse = ReuseNever
	if err := workflow.Validate(); err != nil {
		t.Fatalf("validate operator-only workflow: %v", err)
	}
	workflowFingerprint := mustWorkflowFingerprint(t, workflow)
	emptyInputs := mustInputFingerprint(t, nil)
	now := time.Date(2026, time.July, 14, 10, 0, 0, 0, time.UTC)
	snapshot := ContinuationPlanSnapshot{
		PlanID: "operator-only-plan", CommandID: "operator-only-command", Strategy: StrategyRecompute,
		BaseCheckpoint: testCheckpoint(workflowFingerprint), NextExecutionEpoch: 1, SourceRunID: "run-1",
		TargetRunRelation: RelationSameRunAttempt, SubjectRevisionID: "revision-1", SubjectDigest: SubjectDigest(SHA256Fingerprint([]byte("subject-1"))),
		Nodes: []NodeTransition{
			{NodeID: "source", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"requested"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "verify", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"dependency"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "deliver", FromGeneration: 0, ToGeneration: 0, Disposition: DispositionOperatorOnly, ReasonCodes: []PlanReason{"operator_only_lifecycle_action"}, ExpectedInputFingerprint: emptyInputs},
		},
		Schedule:  []ScheduleBatch{{ID: "source", NodeIDs: []NodeID{"source"}}, {ID: "verify", NodeIDs: []NodeID{"verify"}}},
		ExpiresAt: now.Add(time.Hour),
	}
	if _, err := FreezeContinuationPlan(snapshot, workflow); err != nil {
		t.Fatalf("freeze operator-only continuation plan: %v", err)
	}

	forged := snapshot.Clone()
	forged.Nodes[2].Disposition = DispositionSchedule
	forged.Nodes[2].ToGeneration = 1
	forged.Nodes[2].ReasonCodes = []PlanReason{"forged"}
	forged.Schedule = append(forged.Schedule, ScheduleBatch{ID: "deliver", NodeIDs: []NodeID{"deliver"}})
	forged.ExternalEffectConfirmations = []ExternalEffectConfirmation{{NodeID: "deliver", IdempotencyKey: "forged", Actor: "operator", ConfirmedAt: now}}
	if _, err := FreezeContinuationPlan(forged, workflow); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("operator-only continuation schedule error = %v, want ErrInvalidContinuationPlan", err)
	}
}

func TestContinuationPlanAllowsUnresolvedInputsOnlyForScheduledOrInvalidatedStages(t *testing.T) {
	workflow := testWorkflow(t)
	workflow.Stages[1].Inputs = []ArtifactSpec{{Name: "source_output", SchemaVersion: "v1", Required: true}}
	workflow.Stages[0].Outputs = []ArtifactSpec{{Name: "source_output", SchemaVersion: "v1", Required: true}}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("workflow with required input: %v", err)
	}
	workflowFingerprint := mustWorkflowFingerprint(t, workflow)
	emptyInputs := mustInputFingerprint(t, nil)
	now := time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC)
	base := ContinuationPlanSnapshot{
		PlanID:             "plan-unresolved-inputs",
		CommandID:          "command-unresolved-inputs",
		Strategy:           StrategyRecompute,
		BaseCheckpoint:     testCheckpoint(workflowFingerprint),
		NextExecutionEpoch: 1,
		SourceRunID:        "run-1",
		TargetRunRelation:  RelationSameRunAttempt,
		SubjectRevisionID:  "revision-1",
		SubjectDigest:      SubjectDigest(SHA256Fingerprint([]byte("subject-1"))),
		Nodes: []NodeTransition{
			{NodeID: "source", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"requested"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "verify", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionSchedule, ReasonCodes: []PlanReason{"dependency"}, ExpectedInputFingerprint: emptyInputs},
			{NodeID: "deliver", FromGeneration: 0, ToGeneration: 1, Disposition: DispositionInvalidate, ReasonCodes: []PlanReason{"dependency"}, ExpectedInputFingerprint: emptyInputs},
		},
		Schedule: []ScheduleBatch{
			{ID: "source", NodeIDs: []NodeID{"source"}},
			{ID: "verify", NodeIDs: []NodeID{"verify"}},
		},
		ExpiresAt: now.Add(time.Hour),
	}
	if _, err := FreezeContinuationPlan(base, workflow); err != nil {
		t.Fatalf("scheduled dependency may defer its newly produced input: %v", err)
	}

	preserved := base.Clone()
	preserved.Nodes[1].Disposition = DispositionPreserve
	preserved.Nodes[1].ToGeneration = 0
	preserved.Nodes[1].ReasonCodes = []PlanReason{"reuse"}
	preserved.Schedule = []ScheduleBatch{{ID: "source", NodeIDs: []NodeID{"source"}}}
	if _, err := FreezeContinuationPlan(preserved, workflow); !errors.Is(err, ErrInvalidContinuationPlan) {
		t.Fatalf("preserved stage without required evidence error = %v, want ErrInvalidContinuationPlan", err)
	}
}

func TestInvalidationUsesResourcesLineageAndExternalEffectSafeguard(t *testing.T) {
	workflow := testWorkflow(t)
	emptyInputs := mustInputFingerprint(t, nil)
	states := []StageReuseState{
		{NodeID: "source", Present: true, ArtifactsIntact: true, ExpectedInputFingerprint: emptyInputs},
		{NodeID: "verify", Present: true, ArtifactsIntact: true, ExpectedInputFingerprint: emptyInputs},
		{NodeID: "deliver", Present: true, ArtifactsIntact: true, ExpectedInputFingerprint: emptyInputs},
	}
	plan, err := PlanInvalidation(workflow, InvalidationRequest{
		ChangedResources: []ResourceChange{{Key: "subject/content"}},
		ReuseStates:      states,
	})
	if err != nil {
		t.Fatalf("plan invalidation: %v", err)
	}
	assertImpact(t, plan, "source", ImpactPreserve)
	assertImpact(t, plan, "verify", ImpactInvalidate)
	assertImpact(t, plan, "deliver", ImpactRequiresConfirmation)
	cloned := plan.Clone()
	cloned.Entries[1].Reasons[0] = "mutated"
	original, _ := plan.Entry("verify")
	if len(original.Reasons) > 0 && original.Reasons[0] == "mutated" {
		t.Fatal("invalidation clone mutated original plan")
	}

	states[0].ArtifactsIntact = false
	damaged, err := PlanInvalidation(workflow, InvalidationRequest{ReuseStates: states})
	if err != nil {
		t.Fatalf("plan damaged artifacts: %v", err)
	}
	assertImpact(t, damaged, "source", ImpactInvalidate)
	assertImpact(t, damaged, "verify", ImpactInvalidate)
	assertImpact(t, damaged, "deliver", ImpactRequiresConfirmation)
}

func testBudget() ExecutionBudget {
	return ExecutionBudget{
		TurnTimeout:    10 * time.Second,
		MaxTurns:       2,
		AttemptTimeout: 22 * time.Second,
		MaxAttempts:    2,
		MaxElapsed:     45 * time.Second,
		IdleTimeout:    0,
		StartupGrace:   time.Second,
		ShutdownGrace:  time.Second,
		Backoff:        BackoffPolicy{RetryDelays: []time.Duration{time.Second}},
	}
}

func TestWorkflowRejectsImplicitStageDispatch(t *testing.T) {
	workflow := testWorkflow(t)
	workflow.Stages[0].Dispatch = ""
	if StageDispatchPolicy("").IsAutomatic() {
		t.Fatal("empty dispatch policy was treated as automatic")
	}
	if err := workflow.Validate(); !errors.Is(err, ErrInvalidDescriptor) {
		t.Fatalf("implicit stage dispatch error = %v, want invalid descriptor", err)
	}
}

func testWorkflow(t *testing.T) WorkflowDescriptor {
	t.Helper()
	workflow := WorkflowDescriptor{
		ID:      "example-workflow",
		Version: "v1",
		Stages: []StageDescriptor{
			testStage("source", nil, EffectContentProducer, nil, []ResourceKey{"subject/content"}),
			testStage("verify", []StageKey{"source"}, EffectEvidenceOnly, []ResourceKey{"subject/content"}, []ResourceKey{"evidence/verify"}),
			testStage("deliver", []StageKey{"verify"}, EffectExternalSideEffect, []ResourceKey{"evidence/verify"}, []ResourceKey{"delivery/receipt"}),
		},
	}
	if err := workflow.Validate(); err != nil {
		t.Fatalf("test workflow invalid: %v", err)
	}
	return workflow
}

func testStage(key StageKey, dependencies []StageKey, effect StageEffect, reads, writes []ResourceKey) StageDescriptor {
	return StageDescriptor{
		Key:          key,
		Version:      "v1",
		Plugin:       PluginBinding{ID: "example." + string(key), Version: "1.0.0"},
		Group:        "example",
		Dependencies: dependencies,
		ReadSet:      reads,
		WriteSet:     writes,
		Effect:       effect,
		Dispatch:     StageDispatchAutomatic,
		Budget:       testBudget(),
		Retry:        RetryPolicy{Retryable: []FailureClass{FailureTimeout, FailureNetwork}},
		Verdicts:     VerdictPolicy{Allowed: []Verdict{VerdictPass, VerdictNeedsRepair, VerdictReject, VerdictAdvisory}},
		Reuse:        ReuseWhenInputsMatch,
		Capabilities: CapabilitySet{CapabilityCancel, CapabilityContinue},
	}
}

func testCheckpoint(workflowFingerprint Fingerprint) CheckpointRef {
	return CheckpointRef{
		Sequence:            1,
		ExecutionEpoch:      0,
		SubjectVersion:      1,
		SubjectID:           "subject-1",
		SubjectRevisionID:   "revision-1",
		SubjectDigest:       SubjectDigest(SHA256Fingerprint([]byte("subject-1"))),
		WorkflowFingerprint: workflowFingerprint,
	}
}

func testArtifact(t *testing.T, workflow WorkflowDescriptor, attemptID AttemptID) ArtifactRef {
	t.Helper()
	workflowFingerprint := mustWorkflowFingerprint(t, workflow)
	emptyInputs := mustInputFingerprint(t, nil)
	return ArtifactRef{
		ID:                  "artifact-1",
		ContentDigest:       SHA256Fingerprint([]byte("failure report")),
		SchemaVersion:       "v1",
		RunID:               "run-1",
		StageKey:            "source",
		AttemptID:           attemptID,
		TurnOrdinal:         0,
		WorkflowFingerprint: workflowFingerprint,
		SubjectRevisionID:   "revision-1",
		SubjectDigest:       SubjectDigest(SHA256Fingerprint([]byte("subject-1"))),
		InputFingerprint:    emptyInputs,
		ProducerVersion:     "producer-v1",
		CreatedAt:           time.Date(2026, time.July, 13, 10, 0, 0, 0, time.UTC),
		State:               ArtifactActive,
	}
}

func mustWorkflowFingerprint(t *testing.T, workflow WorkflowDescriptor) Fingerprint {
	t.Helper()
	fingerprint, err := workflow.Fingerprint()
	if err != nil {
		t.Fatalf("workflow fingerprint: %v", err)
	}
	return fingerprint
}

func mustInputFingerprint(t *testing.T, bindings []ArtifactBinding) Fingerprint {
	t.Helper()
	fingerprint, err := FingerprintArtifactBindings(bindings)
	if err != nil {
		t.Fatalf("input fingerprint: %v", err)
	}
	return fingerprint
}

func assertImpact(t *testing.T, plan InvalidationPlan, nodeID NodeID, want InvalidationImpact) {
	t.Helper()
	entry, ok := plan.Entry(nodeID)
	if !ok {
		t.Fatalf("missing invalidation entry for %q", nodeID)
	}
	if entry.Impact != want {
		t.Fatalf("impact for %q = %q, want %q (reasons: %v)", nodeID, entry.Impact, want, entry.Reasons)
	}
}

func reverseResources(values []ResourceKey) []ResourceKey {
	copyValues := append([]ResourceKey(nil), values...)
	for left, right := 0, len(copyValues)-1; left < right; left, right = left+1, right-1 {
		copyValues[left], copyValues[right] = copyValues[right], copyValues[left]
	}
	return copyValues
}

func reverseCapabilities(values CapabilitySet) CapabilitySet {
	copyValues := append(CapabilitySet(nil), values...)
	for left, right := 0, len(copyValues)-1; left < right; left, right = left+1, right-1 {
		copyValues[left], copyValues[right] = copyValues[right], copyValues[left]
	}
	return copyValues
}
