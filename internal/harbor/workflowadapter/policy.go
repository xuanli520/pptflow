package workflowadapter

import (
	"fmt"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// FindingClass is the normalized Harbor policy classification of a completed
// checker finding or an execution failure. Callers must classify facts before
// projecting an outcome; this adapter does not treat an arbitrary error as a
// content verdict.
type FindingClass string

const (
	FindingContentFailure    FindingClass = "content_failure"
	FindingSecurityViolation FindingClass = "security_violation"
	FindingPolicyViolation   FindingClass = "policy_violation"
	FindingWarning           FindingClass = "warning"
	FindingInfrastructure    FindingClass = "infrastructure"
)

// RepairFirstOutcome applies the confirmed repair-first policy:
//
//   - trustworthy deterministic content failures complete with needs_repair;
//   - security and policy violations complete with reject;
//   - warnings complete with advisory; and
//   - unavailable or untrustworthy execution evidence is infra_failed.
//
// Infrastructure callers must provide a concrete workflowkit FailureClass so
// retry admission remains an explicit runtime decision.
func RepairFirstOutcome(class FindingClass, failure workflowkit.FailureClass) (workflowkit.Outcome, error) {
	switch class {
	case FindingContentFailure:
		return validatedOutcome(workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictNeedsRepair})
	case FindingSecurityViolation, FindingPolicyViolation:
		return validatedOutcome(workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictReject})
	case FindingWarning:
		return validatedOutcome(workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictAdvisory})
	case FindingInfrastructure:
		return validatedOutcome(workflowkit.Outcome{Status: workflowkit.StatusInfraFailed, Failure: failure})
	default:
		return workflowkit.Outcome{}, fmt.Errorf("%w: unsupported finding class %q", errInvalidCatalog, class)
	}
}

func validatedOutcome(outcome workflowkit.Outcome) (workflowkit.Outcome, error) {
	if err := outcome.Validate(); err != nil {
		return workflowkit.Outcome{}, fmt.Errorf("%w: repair-first outcome: %v", errInvalidCatalog, err)
	}
	return outcome, nil
}

// HarborResourceMatch gives the kernel's invalidation planner Harbor's
// resource vocabulary semantics. Exact resources match each other; a trailing
// /** matches all descendants on either side so a task-wide repair safely
// invalidates readers of individual task resources.
func HarborResourceMatch(read, changed workflowkit.ResourceKey) bool {
	readValue := strings.TrimSpace(string(read))
	changedValue := strings.TrimSpace(string(changed))
	if readValue == "" || changedValue == "" {
		return false
	}
	if readValue == changedValue {
		return true
	}
	return resourcePatternMatches(readValue, changedValue) || resourcePatternMatches(changedValue, readValue)
}

func resourcePatternMatches(pattern, value string) bool {
	if !strings.HasSuffix(pattern, "/**") {
		return false
	}
	prefix := strings.TrimSuffix(pattern, "**")
	return strings.HasPrefix(value, prefix)
}
