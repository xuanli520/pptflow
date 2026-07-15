# Materialize task

The approved immutable artifacts and the durable `solution_review` decision
are the sole inputs. Invoke the locked Harbor Flow materialization handler to
atomically create exactly one first TaskRevision, seal its snapshot/digest,
and emit `authoring_task_handoff` with schema
`harbor.authoring-task-handoff.v1`. This AuthoringSession Run ends after that
handoff; later CodeEdge verification and packaging run in the separately
created task-bound child Run.
