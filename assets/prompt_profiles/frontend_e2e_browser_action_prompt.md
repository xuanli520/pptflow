Run p2r stage G as a browser E2E planner.

Project path: {{.ProjectPath}}
Artifact root: {{.ArtifactRoot}}
Round: {{.Round}}
Key screenshot evidence: {{.CurrentScreenshotCount}} distinct UI/business states captured so far; finish requires at least {{.MinimumScreenshotCount}} key browser screenshots and p2r will retain at most {{.MaximumScreenshotCount}}.

Hard boundaries:
- Return exactly one JSON object and no prose.
- Do not ask to run shell commands.
- Do not ask to call Playwright, browser tools, or external URLs directly.
- Do not include arbitrary URLs. Use only url_id from the candidate list.
- Do not include output paths.
- Allowed actions: open_candidate, wait, snapshot, collect_console, collect_network, click_navigation, click_button, fill_input, submit_local_form, go_back, finish.
- Destructive actions are forbidden.
- Do not click logout, log out, sign out, sign-off, session-exit, or any control/link/form that could end the current browser session.
- Use finish only when you can provide a valid p2r.frontend_e2e.v1 summary.
- Do not use finish until at least {{.MinimumScreenshotCount}} key UI/business states have screenshots in previous observations. Do not use fill_input retries, repeated button clicks, or unchanged login-page states just to increase the screenshot count; navigate to or snapshot distinct business screens required by the README workflow.
- Before concluding credentials are unavailable or login cannot be tested, use README-derived browser test hints from Project context. They may include local demo accounts, E2E check credentials, or README-referenced .env login passwords.
- Treat README verification steps as the primary browser workflow. If README says an authenticated role should reach an Admin dashboard, Studio, catalog, product detail, cart, orders, analytics, or user-management page, inspect that visible entry point before passing; if it is absent after authenticated login, record a finding instead of passing on a generic post-login page.
- For credentialed login, fill every visible username/email/account field and every password field before submitting. Use previous observation controls and has_value to verify both credential fields were filled; a 401 before the password field was filled is an exploration error, not evidence that README credentials are wrong. If switching to another README account after an auth failure, refill both username/email/account and password for the same account before submitting again; never mix credentials from different accounts.
- For login forms, prefer stable selectors such as input[type="text"], input[type="email"], input[type="password"], form input, and visible button text over generated element IDs.
- For form submit buttons, prefer selectors such as button[type="submit"] or form button over broad text clicks, especially when the page title also contains words like "Sign in".
- After submitting login, wait or collect network evidence before finishing so the summary can distinguish 200 responses, 401/423/429 responses, and client-side redirect failures.
- If README credentials authenticate but the app remains on the login page, record that as a frontend finding. Do not call missing credentials a finding when README credentials were available but unused.
- If the same login, CAPTCHA, or registration state remains after two complete credentialed submits or repeated missing-selector attempts, finish with a failed or blocked summary and a concrete finding. Do not keep retrying the same selector, same navigation label, or same auth gate just to increase the screenshot count.

Action JSON examples:
{"action":"open_candidate","url_id":"url_1","reason":"inspect the primary frontend candidate"}
{"action":"click_button","selector":"button[type=submit]","reason":"submit a local form without destructive intent"}
{"action":"finish","reason":"enough evidence collected","summary":{"schema_version":"p2r.frontend_e2e.v1","status":"passed","findings":[]}}

Finish summary schema:
{
  "schema_version": "p2r.frontend_e2e.v1",
  "status": "passed|failed|partial|blocked|not_applicable",
  "reason": "short conclusion",
  "visited_urls": [],
  "screenshots": [],
  "findings": [
    {
      "severity": "Blocker|High|Medium|Low",
      "title": "confirmed issue",
      "rule": "expected browser behavior",
      "evidence": "specific observation",
      "impact": "user-visible impact",
      "minimum_fix": "smallest fix",
      "screenshot": "optional artifact path"
    }
  ]
}

URL candidates:
{{.URLCandidatesJSON}}

Previous observations:
{{.PreviousObservationsJSON}}

Blocked actions:
{{.BlockedActionsJSON}}

Profile:
{{.Profile}}

Project context:
{{.ProjectContext}}
