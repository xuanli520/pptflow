Review the effectiveness of the project tests.

Rule by project type:
1. If the project includes backend/API services:
   - Verify that API tests truly call the actual project API endpoints.
   - Reject tests that only validate mocks, stubs, patched internals, or bypassed code paths without exercising real API flows.
   - Check whether API tests cover more than 90% of implemented endpoints.
   - Explicitly list untested or weakly tested endpoints.

2. If the project is a pure frontend project with no backend service and no real API interfaces:
   - Exempt the project from API-test existence and API-endpoint coverage requirements.
   - Do not mark the project deficient solely because it has no API tests.
   - Instead, inspect whether frontend tests meaningfully cover core UI behavior, routing, state changes, local storage/session storage behavior, form validation, and key user flows.
   - Mock/local data usage is acceptable in this case unless it hides missing required frontend functionality.

Output constraints:
- Begin the final response immediately with the report's first heading or numbered section.
- Do not include progress updates, tool-use notes, setup narration, environment commentary, or any preamble before the report.
- Keep the report concise and focused.
- Briefly state each issue only; do not provide long narrative explanations.
- For each finding, use at most 3 short bullets: issue, evidence, impact.
- Prefer short sentences and direct conclusions.
- Do not repeat the task description or evaluation criteria in the report.
- Do not include lengthy background analysis, speculation, or generic testing theory.
- If no issue is found for a check, mark it briefly as "No significant issue found".
- Keep the overall report as short as possible while still preserving key conclusions and evidence.
- If p2r supplies a machine-readable JSON contract block, place that block only at the very end of the final response, after all human-readable report sections, and do not write any text after the JSON end marker.

Return only the complete report as the final Codex response. Do not write files or create `.tmp` reports; p2r persists the response to the required artifact paths.
