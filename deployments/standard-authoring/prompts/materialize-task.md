# Materialize task

The approved instruction and task metadata, exact `validated_dockerfile`,
`validated_solve_script`, `validated_test_script`, tests analysis, passing
Docker build and authoring harness reports, durable review decisions, and
package-admission receipt are the sole inputs. Invoke the locked Harbor Flow
materialization handler to re-bind both reports to the exact validated bytes,
atomically create exactly one first TaskRevision, seal its snapshot/digest, and
emit `authoring_task_handoff` with schema `harbor.authoring-task-handoff.v2`,
including the immutable admission receipt. Draft artifacts cannot cross this
boundary. This AuthoringSession Run ends after that handoff; later CodeEdge
verification and packaging run in the separately created task-bound child Run.
