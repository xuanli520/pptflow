package workflowkit

import (
	"errors"
	"fmt"
	"sort"
)

const (
	WorkflowFindingFormat       = "workflowkit.workflow-finding.v1"
	WorkflowFindingVersion      = "1"
	WorkflowRepairLedgerFormat  = "workflowkit.workflow-repair-ledger.v1"
	WorkflowRepairLedgerVersion = "1"

	MaxCandidateRepairRounds = 2
)

var (
	// ErrInvalidWorkflowFinding marks an unbound finding, repair rule, or
	// recovery plan. Findings are immutable data, never free-form agent prose.
	ErrInvalidWorkflowFinding = errors.New("workflowkit: invalid workflow finding")
	// ErrCandidateRepairExhausted marks a legitimate candidate finding after
	// the frozen two-round repair budget has been used.
	ErrCandidateRepairExhausted = errors.New("workflowkit: candidate repair budget exhausted")
)

// WorkflowFinding is a host-validated, durable-safe statement from a
// producing stage. It binds a closed code to evidence and, where required, to
// the immutable candidate snapshot that must be repaired.
type WorkflowFinding struct {
	Format           string      `json:"format"`
	Version          string      `json:"version"`
	Code             string      `json:"code"`
	ProducingStage   StageKey    `json:"producing_stage"`
	EvidenceDigest   Fingerprint `json:"evidence_digest"`
	TargetWriter     StageKey    `json:"target_writer"`
	CandidateDigest  Fingerprint `json:"candidate_digest,omitempty"`
	DiagnosticDigest Fingerprint `json:"diagnostic_digest,omitempty"`
}

// NewWorkflowFinding attaches the fixed v1 identity. A caller must still
// validate it against a frozen WorkflowRepairRule before any recovery occurs.
func NewWorkflowFinding(finding WorkflowFinding) (WorkflowFinding, error) {
	finding.Format = WorkflowFindingFormat
	finding.Version = WorkflowFindingVersion
	if err := finding.Validate(); err != nil {
		return WorkflowFinding{}, err
	}
	return finding, nil
}

func (finding WorkflowFinding) Validate() error {
	if finding.Format != WorkflowFindingFormat || finding.Version != WorkflowFindingVersion {
		return fmt.Errorf("%w: unsupported workflow finding identity", ErrInvalidWorkflowFinding)
	}
	if !validAgentDynamicToolName(finding.Code) {
		return fmt.Errorf("%w: workflow finding code %q is not a closed identifier", ErrInvalidWorkflowFinding, finding.Code)
	}
	if err := validateRequired("finding producing stage", string(finding.ProducingStage), ErrInvalidWorkflowFinding); err != nil {
		return err
	}
	if err := validateRequired("finding target writer", string(finding.TargetWriter), ErrInvalidWorkflowFinding); err != nil {
		return err
	}
	for label, digest := range map[string]Fingerprint{"finding evidence": finding.EvidenceDigest, "candidate": finding.CandidateDigest, "diagnostic": finding.DiagnosticDigest} {
		if digest == "" && label != "finding evidence" {
			continue
		}
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%w: %s digest: %v", ErrInvalidWorkflowFinding, label, err)
		}
	}
	return nil
}

// WorkflowRepairRule is catalog-owned policy for one allowed finding code.
// A repair target is explicit rather than inferred from an agent response.
type WorkflowRepairRule struct {
	FindingCode               string   `json:"finding_code"`
	ProducingStage            StageKey `json:"producing_stage"`
	TargetWriter              StageKey `json:"target_writer"`
	RequiresCandidateSnapshot bool     `json:"requires_candidate_snapshot"`
	ConsumesCandidateRepair   bool     `json:"consumes_candidate_repair"`
}

func (rule WorkflowRepairRule) validate(workflow WorkflowDescriptor) error {
	if !validAgentDynamicToolName(rule.FindingCode) {
		return fmt.Errorf("%w: workflow repair finding code %q is not a closed identifier", ErrInvalidWorkflowFinding, rule.FindingCode)
	}
	producer, found := workflow.Stage(rule.ProducingStage)
	if !found {
		return fmt.Errorf("%w: workflow repair producer %q is not a descriptor stage", ErrInvalidWorkflowFinding, rule.ProducingStage)
	}
	if producer.Effect != EffectEvidenceOnly && producer.Effect != EffectReadOnly && producer.Effect != EffectContentProducer {
		return fmt.Errorf("%w: workflow repair producer %q cannot emit findings", ErrInvalidWorkflowFinding, rule.ProducingStage)
	}
	target, found := workflow.Stage(rule.TargetWriter)
	if !found {
		return fmt.Errorf("%w: workflow repair target %q is not a descriptor stage", ErrInvalidWorkflowFinding, rule.TargetWriter)
	}
	if !target.AutomaticallyDispatchable() || (target.Effect != EffectContentProducer && target.Effect != EffectContentMutator) {
		return fmt.Errorf("%w: workflow repair target %q is not an automatic writer", ErrInvalidWorkflowFinding, rule.TargetWriter)
	}
	if rule.RequiresCandidateSnapshot && !rule.ConsumesCandidateRepair {
		return fmt.Errorf("%w: candidate-bound repair %q must declare candidate repair accounting", ErrInvalidWorkflowFinding, rule.FindingCode)
	}
	return nil
}

// WorkflowRepairLedgerEntry is the compact information a durable audit/event
// store needs to carry between repair plans. One batch may address several
// findings for a target writer, but it consumes at most one candidate-repair
// round for that writer.
type WorkflowRepairLedgerEntry struct {
	Finding                WorkflowFinding `json:"finding"`
	ConsumedCandidateRound bool            `json:"consumed_candidate_round"`
}

func (entry WorkflowRepairLedgerEntry) validate() error {
	return entry.Finding.Validate()
}

// WorkflowRepairLedger is a canonical immutable audit artifact. A workflow
// stores it using ordinary artifact lineage rather than a template-specific
// mutable repair table.
type WorkflowRepairLedger struct {
	Format  string                      `json:"format"`
	Version string                      `json:"version"`
	Entries []WorkflowRepairLedgerEntry `json:"entries"`
}

// NewWorkflowRepairLedger attaches the fixed v1 identity and sorts entries so
// the same set of frozen findings always has the same durable representation.
func NewWorkflowRepairLedger(entries []WorkflowRepairLedgerEntry) (WorkflowRepairLedger, error) {
	ledger := WorkflowRepairLedger{
		Format:  WorkflowRepairLedgerFormat,
		Version: WorkflowRepairLedgerVersion,
		Entries: append([]WorkflowRepairLedgerEntry(nil), entries...),
	}
	sort.Slice(ledger.Entries, func(left, right int) bool {
		return workflowRepairLedgerEntryKey(ledger.Entries[left]) < workflowRepairLedgerEntryKey(ledger.Entries[right])
	})
	if err := ledger.Validate(); err != nil {
		return WorkflowRepairLedger{}, err
	}
	return ledger, nil
}

// Validate rejects ambiguous findings, non-canonical entry order, and multiple
// candidate-round charges for one target writer in the same repair batch.
func (ledger WorkflowRepairLedger) Validate() error {
	if ledger.Format != WorkflowRepairLedgerFormat || ledger.Version != WorkflowRepairLedgerVersion || len(ledger.Entries) == 0 {
		return fmt.Errorf("%w: invalid workflow repair ledger identity or entries", ErrInvalidWorkflowFinding)
	}
	previous := ""
	consumed := make(map[StageKey]struct{})
	for _, entry := range ledger.Entries {
		if err := entry.validate(); err != nil {
			return err
		}
		key := workflowRepairLedgerEntryKey(entry)
		if previous != "" && key <= previous {
			return fmt.Errorf("%w: workflow repair ledger entries are not canonical", ErrInvalidWorkflowFinding)
		}
		previous = key
		if !entry.ConsumedCandidateRound {
			continue
		}
		if _, exists := consumed[entry.Finding.TargetWriter]; exists {
			return fmt.Errorf("%w: workflow repair ledger charges target writer %q more than once", ErrInvalidWorkflowFinding, entry.Finding.TargetWriter)
		}
		consumed[entry.Finding.TargetWriter] = struct{}{}
	}
	return nil
}

func workflowRepairLedgerEntryKey(entry WorkflowRepairLedgerEntry) string {
	finding := entry.Finding
	return string(finding.TargetWriter) + "\x00" + string(finding.ProducingStage) + "\x00" + finding.Code + "\x00" + string(finding.EvidenceDigest) + "\x00" + string(finding.CandidateDigest) + "\x00" + string(finding.DiagnosticDigest)
}

// WorkflowRepairPlan is the generic continuation input for a repair. It
// deliberately requests a new fenced conversation rather than carrying an
// old transcript or writable workspace handle.
type WorkflowRepairPlan struct {
	Finding                    WorkflowFinding `json:"finding"`
	InvalidatedNodes           []NodeID        `json:"invalidated_nodes"`
	Schedule                   []ScheduleBatch `json:"schedule"`
	CandidateRepairRound       int             `json:"candidate_repair_round"`
	RequiresFencedConversation bool            `json:"requires_fenced_conversation"`
}

// Clone returns an independently owned plan.
func (plan WorkflowRepairPlan) Clone() WorkflowRepairPlan {
	plan.InvalidatedNodes = append([]NodeID(nil), plan.InvalidatedNodes...)
	plan.Schedule = cloneCoordinatorScheduleBatches(plan.Schedule)
	return plan
}

// PlanWorkflowRepair validates one finding against frozen rules, computes the
// target writer's dependency/resource-consumer closure, and returns the
// conflict-safe continuation schedule for precisely that closure.
func PlanWorkflowRepair(workflow WorkflowDescriptor, finding WorkflowFinding, rules []WorkflowRepairRule, ledger []WorkflowRepairLedgerEntry) (WorkflowRepairPlan, error) {
	if err := workflow.Validate(); err != nil {
		return WorkflowRepairPlan{}, fmt.Errorf("%w: workflow: %v", ErrInvalidWorkflowFinding, err)
	}
	if err := finding.Validate(); err != nil {
		return WorkflowRepairPlan{}, err
	}
	rule, err := matchingWorkflowRepairRule(workflow, finding, rules)
	if err != nil {
		return WorkflowRepairPlan{}, err
	}
	if rule.RequiresCandidateSnapshot && (finding.CandidateDigest == "" || finding.DiagnosticDigest == "") {
		return WorkflowRepairPlan{}, fmt.Errorf("%w: finding %q requires candidate and diagnostic digests", ErrInvalidWorkflowFinding, finding.Code)
	}
	round, err := candidateRepairRound(finding, rule, ledger)
	if err != nil {
		return WorkflowRepairPlan{}, err
	}
	invalidated := workflowRepairClosure(workflow, finding.TargetWriter)
	schedule, err := workflowRepairSchedule(workflow, invalidated)
	if err != nil {
		return WorkflowRepairPlan{}, err
	}
	return WorkflowRepairPlan{
		Finding: finding, InvalidatedNodes: invalidated, Schedule: schedule,
		CandidateRepairRound: round, RequiresFencedConversation: true,
	}, nil
}

func matchingWorkflowRepairRule(workflow WorkflowDescriptor, finding WorkflowFinding, rules []WorkflowRepairRule) (WorkflowRepairRule, error) {
	var match *WorkflowRepairRule
	for index := range rules {
		rule := rules[index]
		if err := rule.validate(workflow); err != nil {
			return WorkflowRepairRule{}, err
		}
		if rule.FindingCode != finding.Code {
			continue
		}
		if match != nil {
			return WorkflowRepairRule{}, fmt.Errorf("%w: duplicate workflow repair rule for finding code %q", ErrInvalidWorkflowFinding, finding.Code)
		}
		match = &rule
	}
	if match == nil || match.ProducingStage != finding.ProducingStage || match.TargetWriter != finding.TargetWriter {
		return WorkflowRepairRule{}, fmt.Errorf("%w: finding %q does not match a frozen repair rule", ErrInvalidWorkflowFinding, finding.Code)
	}
	return *match, nil
}

func candidateRepairRound(finding WorkflowFinding, rule WorkflowRepairRule, ledger []WorkflowRepairLedgerEntry) (int, error) {
	if !rule.ConsumesCandidateRepair {
		return 0, nil
	}
	used := 0
	for _, entry := range ledger {
		if err := entry.validate(); err != nil {
			return 0, err
		}
		if entry.ConsumedCandidateRound && entry.Finding.TargetWriter == finding.TargetWriter {
			used++
		}
	}
	if used >= MaxCandidateRepairRounds {
		return 0, fmt.Errorf("%w: target writer %q used %d of %d rounds", ErrCandidateRepairExhausted, finding.TargetWriter, used, MaxCandidateRepairRounds)
	}
	return used + 1, nil
}

func workflowRepairClosure(workflow WorkflowDescriptor, target StageKey) []NodeID {
	selected := map[StageKey]struct{}{target: {}}
	changed := true
	for changed {
		changed = false
		for _, stage := range workflow.Stages {
			if !stage.AutomaticallyDispatchable() {
				continue
			}
			if _, exists := selected[stage.Key]; exists {
				continue
			}
			if stageDependsOnSelected(stage, selected) || stageConsumesSelectedWrite(stage, selected, workflow) {
				selected[stage.Key] = struct{}{}
				changed = true
			}
		}
	}
	order := mustTopologicalStages(workflow)
	result := make([]NodeID, 0, len(selected))
	for _, key := range order {
		if _, exists := selected[key]; exists {
			result = append(result, NodeID(key))
		}
	}
	return result
}

func stageDependsOnSelected(stage StageDescriptor, selected map[StageKey]struct{}) bool {
	for _, dependency := range stage.Dependencies {
		if _, found := selected[dependency]; found {
			return true
		}
	}
	return false
}

func stageConsumesSelectedWrite(stage StageDescriptor, selected map[StageKey]struct{}, workflow WorkflowDescriptor) bool {
	for key := range selected {
		producer, _ := workflow.Stage(key)
		if _, found := resourceOverlap(producer.WriteSet, stage.ReadSet); found {
			return true
		}
	}
	return false
}

func workflowRepairSchedule(workflow WorkflowDescriptor, invalidated []NodeID) ([]ScheduleBatch, error) {
	selected := make(map[NodeID]struct{}, len(invalidated))
	for _, nodeID := range invalidated {
		selected[nodeID] = struct{}{}
	}
	repairWorkflow := WorkflowDescriptor{ID: workflow.ID + "-repair", Version: workflow.Version}
	for _, original := range workflow.Stages {
		if _, include := selected[original.Key]; !include {
			continue
		}
		stage := original.Clone()
		stage.Dependencies = nil
		dependencies := make(map[StageKey]struct{})
		for _, dependency := range original.Dependencies {
			if _, included := selected[dependency]; included {
				dependencies[dependency] = struct{}{}
			}
		}
		// A resource consumer outside the original dependency chain must still
		// observe the repaired writer's new output, not a stale candidate.
		for _, producer := range workflow.Stages {
			if producer.Key == original.Key {
				continue
			}
			if _, included := selected[producer.Key]; !included {
				continue
			}
			if _, consumes := resourceOverlap(producer.WriteSet, original.ReadSet); consumes {
				dependencies[producer.Key] = struct{}{}
			}
		}
		for _, key := range workflow.Stages {
			if _, dependency := dependencies[key.Key]; dependency {
				stage.Dependencies = append(stage.Dependencies, key.Key)
			}
		}
		repairWorkflow.Stages = append(repairWorkflow.Stages, stage)
	}
	plan, err := CompileDependencyExecutionPlan(repairWorkflow)
	if err != nil {
		return nil, err
	}
	return append([]ScheduleBatch(nil), plan.Batches...), nil
}

// CanonicalInvalidatedNodes returns a sorted copy for callers that need a
// stable representation outside the dependency-aware plan ordering.
func (plan WorkflowRepairPlan) CanonicalInvalidatedNodes() []NodeID {
	nodes := append([]NodeID(nil), plan.InvalidatedNodes...)
	sort.Slice(nodes, func(left, right int) bool { return nodes[left] < nodes[right] })
	return nodes
}
