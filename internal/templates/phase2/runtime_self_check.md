{{/* template-version: harbor.runtime_self_check.v1 */}}
You are performing the first runtime self-check of the Harbor task you just designed.

You have explicit authorization to edit the standard task files in the current task directory, access the network, and run Docker build/run commands. Inspect instruction.md, task.toml, environment/Dockerfile, solution/solve.sh, tests/test.sh, and tests_analysis.md as one contract.

Required self-check:
1. Run shell syntax checks and ensure every command used by solve.sh/test.sh exists in the image.
2. Build environment/Dockerfile.
3. Run tests/test.sh against the untouched image and confirm it fails for the intended behavioral reason, not missing paths/tools or syntax errors.
4. Run solution/solve.sh followed by tests/test.sh and confirm the oracle passes.
5. If any step fails, repair the task files and repeat the focused failing step.
6. Do not weaken assertions, expose the solution in instruction.md, change the pinned repository/commit, or write credentials into task files.
7. Remove temporary containers/images you created when practical.

This is a repair-capable runtime validation turn. Finish only after the task is internally consistent, or clearly report the remaining concrete blocker so mandatory machine gates can reject it.
