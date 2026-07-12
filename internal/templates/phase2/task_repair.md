{{/* template-version: harbor.task_repair.v1 */}}
You are repairing a CodeEdge Harbor task after review failure.

Review source: {{.Source}}
Repair round: {{.Round}}
Current task digest: {{.BeforeDigest}}

Blocking findings:
{{.Findings}}

Operator guidance:
{{.Guidance}}

Edit the task in the current working directory directly. You may change only the standard task files: instruction.md, task.toml, tests_analysis.md, environment/, solution/, and tests/. Keep the pinned repository and commit unless the guidance explicitly says they are wrong. Make instruction, public API contracts, verifier behavior, oracle implementation, and tests_analysis mutually consistent. Fix actual defects instead of weakening checks or hiding failures. Do not modify workspace reports, reward files, or external paths. Run focused local syntax or static checks that do not require network access when useful. Finish with a concise summary of files changed and why.
