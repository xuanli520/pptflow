package workflowadapter

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// StandardQuotaPolicyID and StandardQuotaPolicyVersion identify the
	// explicitly bounded local-operator quota policy compiled into every
	// standard Harbor workflow. They are frozen in descriptors, manifests, and
	// durable job payloads; callers cannot supply substitute numeric claims.
	StandardQuotaPolicyID      = "harbor.local.operator"
	StandardQuotaPolicyVersion = "1.0.0"

	// StandardAuthoringQuotaPolicyID and Version identify the bounded 3.0
	// source-session authoring policy.
	StandardAuthoringQuotaPolicyID              = "harbor.standard-authoring.local.operator"
	StandardAuthoringContractQuotaPolicyVersion = "3.1.0"
	StandardAuthoringValidationQuotaDimension   = "authoring_validation"
	// StandardAuthoringOutputSubmissionClaimUnits is the fixed number of
	// model-owned validate-and-submit calls reserved for every authoring agent
	// stage. It is versioned with the policy rather than supplied by a Run.
	StandardAuthoringOutputSubmissionClaimUnits int64 = 3
	// StandardAuthoringWorkspaceSubmissionClaimUnits bounds real
	// validate-and-submit iterations for v2 writable workspace stages. Every
	// call is also charged to authoring_validation.
	StandardAuthoringWorkspaceSubmissionClaimUnits int64 = 8
	// StandardAuthoringValidationClaimUnits bounds ReAct validation calls made
	// by each workspace stage independently of model turns and final output
	// submissions.
	StandardAuthoringValidationClaimUnits int64 = 8

	standardTaskStageAttemptLimit         int64 = 120
	standardActorStageAttemptLimit        int64 = 1200
	standardTaskAgentTurnLimit            int64 = 64
	standardActorAgentTurnLimit           int64 = 640
	standardTaskOutputSubmissionLimit     int64 = 64
	standardActorOutputSubmissionLimit    int64 = 640
	standardTaskAuthoringValidationLimit  int64 = 64
	standardActorAuthoringValidationLimit int64 = 640
	standardTaskTrialLimit                int64 = 32
	standardActorTrialLimit               int64 = 320
	standardTaskRepairRoundLimit          int64 = 3
	standardActorRepairRoundLimit         int64 = 30
	standardAuthoringCandidateRepairLimit int64 = 8

	standardStageAttemptClaimUnits int64 = 1
	standardRepairRoundClaimUnits  int64 = 1
)

// QuotaAccountLimit declares both durable account scopes initialized by one
// frozen Harbor quota policy. Limits are bootstrap facts, not caller defaults:
// an existing account may only change through an explicit optimistic grant.
type QuotaAccountLimit struct {
	Dimension       string `json:"dimension"`
	TaskLimitUnits  int64  `json:"task_limit_units"`
	ActorLimitUnits int64  `json:"actor_limit_units"`
}

func (limit QuotaAccountLimit) validate() error {
	if strings.TrimSpace(limit.Dimension) == "" {
		return fmt.Errorf("%w: quota account dimension is required", errInvalidCatalog)
	}
	if limit.TaskLimitUnits <= 0 || limit.ActorLimitUnits <= 0 {
		return fmt.Errorf("%w: quota account limits for %q must be positive", errInvalidCatalog, limit.Dimension)
	}
	return nil
}

// StageQuotaPolicy declares the fully resolved resource claims for one stage.
// Gates carry an explicit empty claim slice because waiting for review is not a
// billable execution. Every executable stage must carry at least one claim.
type StageQuotaPolicy struct {
	StageKey workflowkit.StageKey     `json:"stage_key"`
	Claims   []workflowkit.QuotaClaim `json:"claims"`
}

// Clone returns an independently mutable stage claim declaration.
func (policy StageQuotaPolicy) Clone() StageQuotaPolicy {
	policy.Claims = append([]workflowkit.QuotaClaim(nil), policy.Claims...)
	return policy
}

func (policy StageQuotaPolicy) validate() error {
	if strings.TrimSpace(string(policy.StageKey)) == "" {
		return fmt.Errorf("%w: quota policy stage key is required", errInvalidCatalog)
	}
	if _, err := workflowkit.NormalizeQuotaClaims(policy.Claims); err != nil {
		return fmt.Errorf("%w: stage quota claims for %q: %v", errInvalidCatalog, policy.StageKey, err)
	}
	return nil
}

// QuotaPolicy is Harbor's code-versioned authority for resource claims and
// account bootstrap values. It has no open-ended configuration map and no
// caller-provided numeric fallback.
type QuotaPolicy struct {
	ID            string              `json:"id"`
	Version       string              `json:"version"`
	AccountLimits []QuotaAccountLimit `json:"account_limits"`
	Stages        []StageQuotaPolicy  `json:"stages"`
}

// Clone returns an independent policy snapshot.
func (policy QuotaPolicy) Clone() QuotaPolicy {
	policy.AccountLimits = append([]QuotaAccountLimit(nil), policy.AccountLimits...)
	stages := policy.Stages
	policy.Stages = make([]StageQuotaPolicy, len(stages))
	for index, stage := range stages {
		policy.Stages[index] = stage.Clone()
	}
	return policy
}

// Validate checks policy-local shape. Catalog coverage and gate semantics are
// verified by ValidateFor, because only the catalog knows those Harbor facts.
func (policy QuotaPolicy) Validate() error {
	if strings.TrimSpace(policy.ID) == "" {
		return fmt.Errorf("%w: quota policy id is required", errInvalidCatalog)
	}
	if strings.TrimSpace(policy.Version) == "" {
		return fmt.Errorf("%w: quota policy version is required", errInvalidCatalog)
	}
	if len(policy.AccountLimits) == 0 {
		return fmt.Errorf("%w: quota policy account limits are required", errInvalidCatalog)
	}
	if len(policy.Stages) == 0 {
		return fmt.Errorf("%w: quota policy stage declarations are required", errInvalidCatalog)
	}
	dimensions := make(map[string]struct{}, len(policy.AccountLimits))
	for _, limit := range policy.AccountLimits {
		if err := limit.validate(); err != nil {
			return err
		}
		if _, duplicate := dimensions[limit.Dimension]; duplicate {
			return fmt.Errorf("%w: duplicate quota account dimension %q", errInvalidCatalog, limit.Dimension)
		}
		dimensions[limit.Dimension] = struct{}{}
	}
	stages := make(map[workflowkit.StageKey]struct{}, len(policy.Stages))
	for _, stage := range policy.Stages {
		if err := stage.validate(); err != nil {
			return err
		}
		if _, duplicate := stages[stage.StageKey]; duplicate {
			return fmt.Errorf("%w: duplicate quota policy stage %q", errInvalidCatalog, stage.StageKey)
		}
		stages[stage.StageKey] = struct{}{}
		for _, claim := range stage.Claims {
			if _, configured := dimensions[claim.Dimension]; !configured {
				return fmt.Errorf("%w: stage %q claims unconfigured quota dimension %q", errInvalidCatalog, stage.StageKey, claim.Dimension)
			}
		}
	}
	return nil
}

// ValidateFor verifies exact catalog coverage. A policy cannot silently start
// charging a new node, omit a node, or charge a durable review wait as work.
func (policy QuotaPolicy) ValidateFor(catalog StageCatalog) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := catalog.Validate(); err != nil {
		return err
	}
	rules := make(map[workflowkit.StageKey]StageQuotaPolicy, len(policy.Stages))
	for _, rule := range policy.Stages {
		rules[rule.StageKey] = rule
	}
	for _, stage := range catalog.Stages {
		rule, present := rules[stage.Key]
		if !present {
			return fmt.Errorf("%w: quota policy omits Harbor stage %q", errInvalidCatalog, stage.Key)
		}
		if stage.IsGate() && len(rule.Claims) != 0 {
			return fmt.Errorf("%w: quota policy gate %q must not claim execution quota", errInvalidCatalog, stage.Key)
		}
		if !stage.IsGate() && len(rule.Claims) == 0 {
			return fmt.Errorf("%w: quota policy executable stage %q must claim quota", errInvalidCatalog, stage.Key)
		}
	}
	for _, rule := range policy.Stages {
		if _, present := catalog.Stage(rule.StageKey); !present {
			return fmt.Errorf("%w: quota policy contains unknown Harbor stage %q", errInvalidCatalog, rule.StageKey)
		}
	}
	return nil
}

func (policy QuotaPolicy) canonical() (QuotaPolicy, error) {
	if err := policy.Validate(); err != nil {
		return QuotaPolicy{}, err
	}
	canonical := policy.Clone()
	for index := range canonical.Stages {
		claims, err := workflowkit.NormalizeQuotaClaims(canonical.Stages[index].Claims)
		if err != nil {
			return QuotaPolicy{}, err
		}
		canonical.Stages[index].Claims = claims
	}
	sort.Slice(canonical.AccountLimits, func(left, right int) bool {
		return canonical.AccountLimits[left].Dimension < canonical.AccountLimits[right].Dimension
	})
	sort.Slice(canonical.Stages, func(left, right int) bool {
		return canonical.Stages[left].StageKey < canonical.Stages[right].StageKey
	})
	return canonical, nil
}

// Fingerprint identifies every numeric account limit and every stage claim.
func (policy QuotaPolicy) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := policy.canonical()
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: encode quota policy: %v", errInvalidCatalog, err)
	}
	return workflowkit.FingerprintBytes("harbor.workflowadapter.quota-policy.v1", encoded)
}

// ResolvedQuotaPolicy is the canonical immutable snapshot persisted alongside
// a descriptor. Claims are duplicated in the descriptor so a runtime can read
// a stage in isolation; this policy snapshot carries the account bootstrap
// limits that cannot belong to a task until admission.
type ResolvedQuotaPolicy struct {
	ID            string                  `json:"id"`
	Version       string                  `json:"version"`
	Fingerprint   workflowkit.Fingerprint `json:"fingerprint"`
	AccountLimits []QuotaAccountLimit     `json:"account_limits"`
	Stages        []StageQuotaPolicy      `json:"stages"`
}

// Clone returns an independent frozen policy snapshot.
func (policy ResolvedQuotaPolicy) Clone() ResolvedQuotaPolicy {
	policy.AccountLimits = append([]QuotaAccountLimit(nil), policy.AccountLimits...)
	stages := policy.Stages
	policy.Stages = make([]StageQuotaPolicy, len(stages))
	for index, stage := range stages {
		policy.Stages[index] = stage.Clone()
	}
	return policy
}

// Validate verifies a resolved policy and its self-authenticating fingerprint.
func (policy ResolvedQuotaPolicy) Validate() error {
	base := QuotaPolicy{ID: policy.ID, Version: policy.Version, AccountLimits: policy.AccountLimits, Stages: policy.Stages}
	if err := base.Validate(); err != nil {
		return err
	}
	fingerprint, err := base.Fingerprint()
	if err != nil {
		return err
	}
	if policy.Fingerprint != fingerprint {
		return fmt.Errorf("%w: resolved quota policy fingerprint mismatch", errInvalidCatalog)
	}
	return nil
}

// ClaimsFor returns a copy of one stage's exact frozen policy claims.
func (policy ResolvedQuotaPolicy) ClaimsFor(key workflowkit.StageKey) ([]workflowkit.QuotaClaim, bool) {
	for _, stage := range policy.Stages {
		if stage.StageKey == key {
			return append([]workflowkit.QuotaClaim(nil), stage.Claims...), true
		}
	}
	return nil, false
}

// ValidateForDescriptor proves that descriptor-local claim copies are exactly
// the policy snapshot, so a tampered manifest cannot reserve different quota
// units than it displays or dispatches.
func (policy ResolvedQuotaPolicy) ValidateForDescriptor(descriptor workflowkit.WorkflowDescriptor) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	if err := descriptor.Validate(); err != nil {
		return err
	}
	if len(policy.Stages) != len(descriptor.Stages) {
		return fmt.Errorf("%w: quota policy stage count does not match descriptor", errInvalidCatalog)
	}
	for _, stage := range descriptor.Stages {
		claims, present := policy.ClaimsFor(stage.Key)
		if !present {
			return fmt.Errorf("%w: resolved quota policy omits descriptor stage %q", errInvalidCatalog, stage.Key)
		}
		expected, err := workflowkit.NormalizeQuotaClaims(claims)
		if err != nil {
			return err
		}
		actual, err := workflowkit.NormalizeQuotaClaims(stage.QuotaClaims)
		if err != nil {
			return err
		}
		if !sameQuotaClaims(expected, actual) {
			return fmt.Errorf("%w: descriptor stage %q quota claims differ from frozen policy", errInvalidCatalog, stage.Key)
		}
	}
	return nil
}

func sameQuotaClaims(left, right []workflowkit.QuotaClaim) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// ResolveFor canonicalizes the code policy for one complete Harbor catalog.
func (policy QuotaPolicy) ResolveFor(catalog StageCatalog) (ResolvedQuotaPolicy, error) {
	if err := policy.ValidateFor(catalog); err != nil {
		return ResolvedQuotaPolicy{}, err
	}
	canonical, err := policy.canonical()
	if err != nil {
		return ResolvedQuotaPolicy{}, err
	}
	fingerprint, err := canonical.Fingerprint()
	if err != nil {
		return ResolvedQuotaPolicy{}, err
	}
	stages := make([]StageQuotaPolicy, len(canonical.Stages))
	for index, stage := range canonical.Stages {
		stages[index] = stage.Clone()
	}
	return ResolvedQuotaPolicy{
		ID:            canonical.ID,
		Version:       canonical.Version,
		Fingerprint:   fingerprint,
		AccountLimits: append([]QuotaAccountLimit(nil), canonical.AccountLimits...),
		Stages:        stages,
	}, nil
}

// StandardQuotaPolicy returns the confirmed explicit local-operator baseline:
// 120/1200 stage attempts, 64/640 agent turns, 32/320 logical trials, and
// 3/30 repair rounds for task/actor scopes respectively. The policy declares
// no token, API-call, wall-time, or concurrency-slot dimensions; accepting
// those without a durable measurement contract would create a hidden numeric
// default. Concurrency remains a separately fenced dispatcher capacity lease.
func StandardQuotaPolicy() QuotaPolicy {
	catalog := StandardStageCatalog()
	stages := make([]StageQuotaPolicy, 0, len(catalog.Stages))
	for _, stage := range catalog.Stages {
		claims := standardClaimsForStage(stage)
		stages = append(stages, StageQuotaPolicy{StageKey: stage.Key, Claims: claims})
	}
	return QuotaPolicy{
		ID:      StandardQuotaPolicyID,
		Version: StandardQuotaPolicyVersion,
		AccountLimits: []QuotaAccountLimit{
			{Dimension: "stage_attempt", TaskLimitUnits: standardTaskStageAttemptLimit, ActorLimitUnits: standardActorStageAttemptLimit},
			{Dimension: "agent_turn", TaskLimitUnits: standardTaskAgentTurnLimit, ActorLimitUnits: standardActorAgentTurnLimit},
			{Dimension: "trial", TaskLimitUnits: standardTaskTrialLimit, ActorLimitUnits: standardActorTrialLimit},
			{Dimension: "repair_round", TaskLimitUnits: standardTaskRepairRoundLimit, ActorLimitUnits: standardActorRepairRoundLimit},
		},
		Stages: stages,
	}
}

// StandardAuthoringContractQuotaPolicy returns the sole 3.0 source-session
// policy. Candidate corrections are explicitly capped at eight rounds; host,
// environment, and infrastructure faults do not reserve that account.
func StandardAuthoringContractQuotaPolicy() QuotaPolicy {
	catalog := StandardAuthoringContractStageCatalog()
	stages := make([]StageQuotaPolicy, 0, len(catalog.Stages))
	for _, stage := range catalog.Stages {
		claims := standardClaimsForStage(stage)
		if stage.AgentRole != nil {
			submissionUnits := StandardAuthoringOutputSubmissionClaimUnits
			if stage.AgentRole.RoleID == workflowkit.AgentRoleAuthor {
				submissionUnits = StandardAuthoringWorkspaceSubmissionClaimUnits
			}
			claims = append(claims, standardQuotaClaim("output_submission", submissionUnits))
		}
		if stage.Key == workflowkit.StageKey(HostCandidateVerify) {
			claims = append(claims, standardQuotaClaim(StandardAuthoringValidationQuotaDimension, StandardAuthoringValidationClaimUnits))
		}
		if stage.Key == workflowkit.StageKey(AuthoringRepair) {
			claims = append(claims, standardQuotaClaim("repair_round", standardRepairRoundClaimUnits))
		}
		stages = append(stages, StageQuotaPolicy{StageKey: stage.Key, Claims: claims})
	}
	return QuotaPolicy{
		ID:      StandardAuthoringQuotaPolicyID,
		Version: StandardAuthoringContractQuotaPolicyVersion,
		AccountLimits: []QuotaAccountLimit{
			{Dimension: "stage_attempt", TaskLimitUnits: standardTaskStageAttemptLimit, ActorLimitUnits: standardActorStageAttemptLimit},
			{Dimension: "agent_turn", TaskLimitUnits: standardTaskAgentTurnLimit, ActorLimitUnits: standardActorAgentTurnLimit},
			{Dimension: "output_submission", TaskLimitUnits: standardTaskOutputSubmissionLimit, ActorLimitUnits: standardActorOutputSubmissionLimit},
			{Dimension: StandardAuthoringValidationQuotaDimension, TaskLimitUnits: standardTaskAuthoringValidationLimit, ActorLimitUnits: standardActorAuthoringValidationLimit},
			{Dimension: "repair_round", TaskLimitUnits: standardAuthoringCandidateRepairLimit, ActorLimitUnits: standardActorRepairRoundLimit},
		},
		Stages: stages,
	}
}

func standardClaimsForStage(stage StageDefinition) []workflowkit.QuotaClaim {
	if stage.IsGate() {
		return []workflowkit.QuotaClaim{}
	}
	claims := []workflowkit.QuotaClaim{standardQuotaClaim("stage_attempt", standardStageAttemptClaimUnits)}
	if _, agentStage := standardAgentQuotaStages[stage.Key]; agentStage {
		claims = append(claims, standardQuotaClaim("agent_turn", int64(stage.RequiredTurns)))
	}
	switch stage.Key {
	case workflowkit.StageKey(TaskRepair):
		claims = append(claims, standardQuotaClaim("repair_round", standardRepairRoundClaimUnits))
	}
	return claims
}

func standardQuotaClaim(dimension string, units int64) workflowkit.QuotaClaim {
	return workflowkit.QuotaClaim{Dimension: dimension, Units: units, ReclaimPolicy: workflowkit.ReclaimUnused}
}

var standardAgentQuotaStages = map[workflowkit.StageKey]struct{}{
	workflowkit.StageKey(RepoAnalyze):             {},
	workflowkit.StageKey(TaskDesign):              {},
	workflowkit.StageKey(GenerateTaskFiles):       {},
	workflowkit.StageKey(InstructionGen):          {},
	workflowkit.StageKey(TaskTOMLGen):             {},
	workflowkit.StageKey(DockerfileGen):           {},
	workflowkit.StageKey(DockerfileBuildValidate): {},
	workflowkit.StageKey(SolveGen):                {},
	workflowkit.StageKey(TestGen):                 {},
	workflowkit.StageKey(AuthoringHarness):        {},
	workflowkit.StageKey(TestsAnalysis):           {},
	workflowkit.StageKey(TaskRepair):              {},
	workflowkit.StageKey(RepoStructureResearch):   {},
	workflowkit.StageKey(TestRuntimeResearch):     {},
	workflowkit.StageKey(VerifierThreatResearch):  {},
	workflowkit.StageKey(TaskSynthesis):           {},
	workflowkit.StageKey(AuthoringLoop):           {},
	workflowkit.StageKey(TestQualityCritic):       {},
	workflowkit.StageKey(SolutionIntegrityCritic): {},
	workflowkit.StageKey(AuthoringRepair):         {},
}
