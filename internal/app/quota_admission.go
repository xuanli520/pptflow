package app

import (
	"errors"
	"fmt"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// ErrFrozenQuotaAdmission marks an attempt to construct an admission request
// from anything other than the exact policy claims frozen with a stage.
var ErrFrozenQuotaAdmission = errors.New("quota admission: frozen policy and descriptor mismatch")

// FrozenStageQuotaAdmission is the runtime-facing, scope-free part of a
// task+actor admission request. Durable runtimes add the concrete task, actor,
// worker owner, lease TTL, and idempotency identity; they must not add, remove,
// or alter resource claims or account limits.
type FrozenStageQuotaAdmission struct {
	Policy            store.QuotaPolicyBinding
	BootstrapAccounts []store.QuotaAccountBootstrap
	Claims            []store.TaskActorQuotaClaim
}

// BuildFrozenStageQuotaAdmission translates a validated Harbor policy snapshot
// into Store's generic task+actor admission facts. The policy's per-stage
// claims must exactly match the descriptor copy so a job payload cannot charge
// different quota from its frozen run manifest.
func BuildFrozenStageQuotaAdmission(policy workflowadapter.ResolvedQuotaPolicy, stage workflowkit.StageDescriptor) (FrozenStageQuotaAdmission, error) {
	if err := policy.Validate(); err != nil {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: validate policy: %v", ErrFrozenQuotaAdmission, err)
	}
	if err := stage.Validate(); err != nil {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: validate stage: %v", ErrFrozenQuotaAdmission, err)
	}
	expected, present := policy.ClaimsFor(stage.Key)
	if !present {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: policy omits stage %q", ErrFrozenQuotaAdmission, stage.Key)
	}
	expected, err := workflowkit.NormalizeQuotaClaims(expected)
	if err != nil {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: normalize policy claims: %v", ErrFrozenQuotaAdmission, err)
	}
	actual, err := workflowkit.NormalizeQuotaClaims(stage.QuotaClaims)
	if err != nil {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: normalize descriptor claims: %v", ErrFrozenQuotaAdmission, err)
	}
	if !equalFrozenStageClaims(expected, actual) {
		return FrozenStageQuotaAdmission{}, fmt.Errorf("%w: stage %q quota claims differ", ErrFrozenQuotaAdmission, stage.Key)
	}

	admission := FrozenStageQuotaAdmission{
		Policy: store.QuotaPolicyBinding{
			PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyFingerprint: string(policy.Fingerprint),
		},
		BootstrapAccounts: make([]store.QuotaAccountBootstrap, 0, len(policy.AccountLimits)),
		Claims:            make([]store.TaskActorQuotaClaim, 0, len(actual)),
	}
	for _, limit := range policy.AccountLimits {
		admission.BootstrapAccounts = append(admission.BootstrapAccounts, store.QuotaAccountBootstrap{
			Dimension: limit.Dimension, TaskLimitUnits: limit.TaskLimitUnits, ActorLimitUnits: limit.ActorLimitUnits,
		})
	}
	for _, claim := range actual {
		admission.Claims = append(admission.Claims, store.TaskActorQuotaClaim{
			Dimension: claim.Dimension, Units: claim.Units, ReclaimPolicy: store.QuotaReclaimPolicy(claim.ReclaimPolicy),
		})
	}
	return admission, nil
}

func equalFrozenStageClaims(left, right []workflowkit.QuotaClaim) bool {
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
