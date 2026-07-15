# CodeEdge Phase-1 parent deployment

This directory is the independent deployment bundle for the complete
`harbor.codeedge-phase1@2.2.0` parent workflow.  It is deliberately separate
from `../codeedge-evaluator-child/`: the parent performs the local task
preflight, controlled verification, durable reviews, evidence adoption and
package authorization; the child alone owns the external Qwen and Opus
pass@4 effects.

`operation-catalog.v1.json` is source-controlled policy.  A production build
must install a generated `operation-catalog.lock.json` beside it.  That lock
must contain both `codeedge_phase1_execution_profile` and
`codeedge_phase1_final_compliance_policy`, plus the exact local executable
attestations for Docker-backed stages.  Missing, substituted, or evaluator
child-only material is rejected before a parent Run can start.

The parent has no model endpoint or credential capability.  Model credentials
are restricted to the separately locked evaluator child bundle.
