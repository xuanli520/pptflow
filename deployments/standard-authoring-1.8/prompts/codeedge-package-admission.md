# CodeEdge Package Admission

This deterministic Harbor operation compiles the frozen instruction and task
metadata with `validated_dockerfile`, `validated_solve_script`,
`validated_test_script`, and tests analysis. It verifies that the host-owned
Docker build and authoring harness reports are passing evidence for those exact
artifact bytes, injects immutable source provenance, and validates the
canonical task package against the pinned CodeEdge preflight contract. It
performs no model turn and never accepts draft artifacts in place of validated
ones.
