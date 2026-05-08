Review the effectiveness of the project tests, with an absolute **hard requirement on API endpoint test coverage**.

**Rule by project type**

1. **If the project includes backend/API services:**
   - **CRITICAL: API endpoint test coverage MUST be greater than 90%.**  
     If coverage is ≤ 90%, the project **automatically fails** this review, regardless of any other quality metrics.
   - Verify that API tests truly call the actual project API endpoints (no mocks, stubs, or patched internals that bypass real flows).
   - Explicitly list every implemented endpoint, indicate whether it is covered, and calculate the exact coverage percentage.
   - List all untested or weakly tested endpoints that cause the coverage to fall at or below 90%.
   - If coverage > 90% but some endpoints are still untested, note them but the hard fail threshold is 90%.

2. **If the project is a pure frontend project with no backend service and no real API interfaces:**
   - Exempt from the API‑test existence and coverage requirement. Do **not** mark it deficient for lacking API tests.
   - Instead, inspect whether frontend tests meaningfully cover core UI behaviour, routing, state changes, local/session storage behaviour, form validation, and key user flows.
   - Mock/local data is acceptable unless it hides missing required frontend functionality.

**Output constraints**
- Begin the final response immediately with the analysis heading.
- No preamble, progress updates, or narration.
- For each finding use at most 3 short bullets: issue, evidence, impact.
- If the API endpoint coverage hard fail is triggered, clearly state **FAIL** and give the exact percentage with the list of uncovered endpoints.
- Do not speculate; base all conclusions on static evidence from the repository.
- If no issue is found and coverage > 90%, state “Pass” and briefly summarise the coverage level.
- Keep the report as short as possible while preserving key conclusions.

Return only the complete report. Do not write files; p2r persists the response.