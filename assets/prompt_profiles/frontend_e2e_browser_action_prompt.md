Run p2r stage G as a browser E2E planner.

Project path: {{.ProjectPath}}
Artifact root: {{.ArtifactRoot}}
Round: {{.Round}}
Key screenshot evidence: {{.CurrentScreenshotCount}} distinct UI/business states captured so far; passed conclusions need strong distinct browser evidence, and p2r will retain at most {{.MaximumScreenshotCount}} screenshots. Failed, blocked, or partial conclusions may finish with fewer screenshots when previous observations show a concrete product blocker, auth boundary, or browser tool error.

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
- You are the browser E2E operator. p2r will not automatically click login, submit forms, navigate, or capture extra snapshots for you; every browser operation must be the single JSON action you choose for this round.
- Do not use finish with status=passed until previous observations show enough distinct UI/business evidence. Do not use fill_input retries, repeated button clicks, or unchanged login-page states just to increase the screenshot count; navigate to or snapshot distinct business screens when safe and useful.
- Once previous observations show a rendered product workflow with multiple distinct successful business screens or business API calls, finish with status=passed unless there is an unrecovered user-visible product failure. Do not return partial/failed only because one optional selector or route failed after another safe path reached the same or broader workflow.
- If previous observations contain p2r browser action tool errors, selector timeouts, wrapper launch failures, or other non-product tool failures, react once with a safer action when there is an obvious safe alternative. If no safe alternative is likely to improve evidence, finish with failed, blocked, or partial and explain the tool boundary from the observations.
- If previous observation metadata shows a native browser prompt or confirm dialog was dismissed because action.value was missing, do not treat the unchanged page as product failure. If the dialog belongs to a safe README/user workflow, retry the same safe click_button, click_navigation, or submit_local_form action with value set to the dialog answer. For confirm dialogs use an explicit value such as "accept" or "cancel".
- Before concluding credentials are unavailable or login cannot be tested, use README-derived browser test hints from Project context. They may include local demo accounts, E2E check credentials, or README-referenced .env login passwords.
- Authentication is not a universal success requirement. If the delivered app exposes a public product workflow, multi-state interactive UI, or clear business API evidence without login, assess that workflow directly and pass or fail from that evidence. Try login only when it is relevant to the documented workflow or visible app boundary.
- Treat README verification steps as the primary browser workflow. If README says an authenticated role should reach an Admin dashboard, Studio, catalog, product detail, cart, orders, analytics, or user-management page, inspect that visible entry point before passing; if it is absent after authenticated login, record a finding instead of passing on a generic post-login page.
- For credentialed login, fill every visible username/email/account field and every password field before submitting. Use previous observation controls and has_value to verify both credential fields were filled; a 401 before the password field was filled is an exploration error, not evidence that README credentials are wrong. Treat each README table/list row as one credential pair; if switching to another README account after an auth failure, refill both username/email/account and password from the same row/account before submitting again. Never mix credentials from different accounts.
- For login forms, prefer stable selectors such as input[type="text"], input[type="email"], input[type="password"], form input, and observed visible button text over generated element IDs.
- For submit buttons, use the observed controls. Prefer button[type="submit"] when the button has submit type; if the observation shows only a visible button label such as "Sign in" or "Register", use click_button with the text field instead of inventing a selector.
- After submitting login, wait or collect network evidence before finishing so the summary can distinguish 200 responses, 401/423/429 responses, and client-side redirect failures.
- If README credentials authenticate but the app remains on the login page, record that as a frontend finding. Do not call missing credentials a finding when README credentials were available but unused.
- If the same login, CAPTCHA, or registration state remains after two complete credentialed submits or repeated missing-selector attempts, finish with a failed, blocked, or partial summary and a concrete finding. Do not keep retrying the same selector, same navigation label, or same auth gate just to increase the screenshot count.
- Failed, blocked, or partial summaries require a concrete observation-backed blocker. A recovered selector timeout, followed by successful navigation/API evidence for the product workflow, should be mentioned in the reason at most, not reported as a blocking finding.

Action JSON examples:
{"action":"open_candidate","url_id":"url_1","reason":"inspect the primary frontend candidate"}
{"action":"click_button","selector":"button[type=submit]","reason":"submit a local form without destructive intent"}
{"action":"click_button","text":"+ Page","value":"Home","reason":"answer the safe native page-title prompt opened by the add-page control"}
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
