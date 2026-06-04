package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/executor"
)

type PlaywrightWrapper struct {
	Exec   executor.CommandRunner
	Node   string
	Policy Policy
}

type wrapperRequest struct {
	Action Action `json:"action"`
	Policy Policy `json:"policy"`
}

func NewPlaywrightWrapper(exec executor.CommandRunner, node string, policy Policy) PlaywrightWrapper {
	return PlaywrightWrapper{Exec: exec, Node: node, Policy: policy}
}

func (w PlaywrightWrapper) Run(ctx context.Context, action Action, timeout time.Duration) (Observation, error) {
	exec := w.Exec
	if exec == nil {
		exec = executor.New()
	}
	node := strings.TrimSpace(w.Node)
	if node == "" {
		found, err := exec.LookPath("node")
		if err != nil {
			return Observation{}, err
		}
		node = found
	}
	root := filepath.Clean(w.Policy.ArtifactRoot)
	if root == "" || root == "." {
		return Observation{}, fmt.Errorf("artifact root is required")
	}
	stateDir := filepath.Join(root, "browser_runtime")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return Observation{}, err
	}
	policy := w.Policy
	if policy.StorageStatePath == "" {
		policy.StorageStatePath = filepath.Join(stateDir, "storage_state.json")
	}
	if policy.LastURLPath == "" {
		policy.LastURLPath = filepath.Join(stateDir, "last_url.txt")
	}
	if policy.ScreenshotPath == "" {
		policy.ScreenshotPath = filepath.Join(root, "frontend_e2e_screenshot.png")
	}
	for _, path := range []string{policy.StorageStatePath, policy.LastURLPath, policy.ScreenshotPath} {
		if !pathWithinRoot(path, root) {
			return Observation{}, fmt.Errorf("browser artifact path escapes root: %s", path)
		}
	}
	requestPath := filepath.Join(stateDir, "request.json")
	scriptPath := filepath.Join(stateDir, "playwright_action.js")
	payload, err := json.Marshal(wrapperRequest{Action: action, Policy: policy})
	if err != nil {
		return Observation{}, err
	}
	if err := os.WriteFile(requestPath, payload, 0o644); err != nil {
		return Observation{}, err
	}
	if err := os.WriteFile(scriptPath, []byte(playwrightActionScript), 0o644); err != nil {
		return Observation{}, err
	}
	result := exec.Run(ctx, timeout, root, nil, node, scriptPath, requestPath)
	if result.Err != nil {
		return Observation{}, fmt.Errorf("playwright action failed: %s", strings.TrimSpace(firstNonEmpty(result.Stderr, result.Stdout, result.Err.Error())))
	}
	var observation Observation
	if err := json.Unmarshal([]byte(result.Stdout), &observation); err != nil {
		return Observation{}, fmt.Errorf("playwright observation JSON invalid: %w", err)
	}
	return sanitizeObservation(observation), nil
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

const playwrightActionScript = `
const fs = require('fs');
const { chromium } = require('playwright');

const request = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
const action = request.action || {};
const policy = request.policy || {};
const allowed = new Set(policy.allowlist_origins || []);
const consoleErrors = [];
const pageErrors = [];
const networkIssues = [];
const blockedRequests = [];

function trim(value, limit) {
  value = String(value || '').replace(/\s+/g, ' ').trim();
  return value.length > limit ? value.slice(0, limit) : value;
}

function originOf(raw) {
  try { return new URL(raw).origin; } catch (_) { return ''; }
}

function safeRead(path) {
  try { return fs.readFileSync(path, 'utf8').trim(); } catch (_) { return ''; }
}

function safeURL(raw) {
  try {
    const value = new URL(raw);
    value.search = '';
    value.hash = '';
    return value.toString();
  } catch (_) {
    return '';
  }
}

async function collect(page, ok, error) {
  let title = '';
  let visibleText = '';
  let controls = [];
  try { title = await page.title(); } catch (_) {}
  try { visibleText = trim(await page.locator('body').innerText({ timeout: 1000 }), 4000); } catch (_) {}
  try {
    controls = await page.evaluate(() => {
      const items = [];
      const push = (role, el) => {
        const text = (el.innerText || el.getAttribute('aria-label') || el.getAttribute('placeholder') || '').replace(/\s+/g, ' ').trim();
        items.push({
          role,
          text: text.slice(0, 160),
          selector: el.id ? '#' + el.id : '',
          name: (el.getAttribute('name') || '').slice(0, 120),
          placeholder: (el.getAttribute('placeholder') || '').slice(0, 160),
          type: (el.getAttribute('type') || '').slice(0, 80)
        });
      };
      document.querySelectorAll('a,button,input,textarea,select').forEach((el) => {
        const tag = el.tagName.toLowerCase();
        if (tag === 'a') push('link', el);
        else if (tag === 'button') push('button', el);
        else push('input', el);
      });
      return items.slice(0, 80);
    });
  } catch (_) {}
  let screenshotPath = '';
  try {
    if (policy.screenshot_path) {
      await page.screenshot({ path: policy.screenshot_path, fullPage: true });
      screenshotPath = policy.screenshot_path;
    }
  } catch (_) {}
  const currentURL = page.url();
  if (policy.last_url_path && currentURL && currentURL !== 'about:blank') {
    try { fs.writeFileSync(policy.last_url_path, safeURL(currentURL)); } catch (_) {}
  }
  try {
    if (policy.storage_state_path) {
      await page.context().storageState({ path: policy.storage_state_path });
    }
  } catch (_) {}
  return {
    action: action.name,
    ok,
    current_url: currentURL,
    title,
    visible_text: visibleText,
    controls,
    console_errors: consoleErrors.slice(0, 80),
    page_errors: pageErrors.slice(0, 80),
    network_issues: networkIssues.slice(0, 120),
    blocked_requests: blockedRequests.slice(0, 120),
    screenshot_path: screenshotPath,
    error: error || ''
  };
}

(async () => {
  let browser;
  try {
    browser = await chromium.launch({ headless: true });
    const contextOptions = {};
    if (policy.storage_state_path && fs.existsSync(policy.storage_state_path)) {
      contextOptions.storageState = policy.storage_state_path;
    }
    const context = await browser.newContext(contextOptions);
    await context.route('**/*', async (route) => {
      const raw = route.request().url();
      let parsed;
      try { parsed = new URL(raw); } catch (_) { return route.abort('blockedbyclient'); }
      if (['about:', 'data:', 'blob:'].includes(parsed.protocol) || allowed.has(parsed.origin)) {
        return route.continue();
      }
      blockedRequests.push({ url: raw, origin: parsed.origin });
      return route.abort('blockedbyclient');
    });
    const page = await context.newPage();
    page.on('console', (msg) => {
      if (msg.type() === 'error') consoleErrors.push(trim(msg.text(), 1000));
    });
    page.on('pageerror', (err) => pageErrors.push(trim(err.message, 1000)));
    page.on('response', (response) => {
      const status = response.status();
      if (status >= 400) networkIssues.push({ url: response.url(), status });
    });
    page.on('requestfailed', (request) => {
      const raw = request.url();
      if (blockedRequests.some((item) => item.url === raw)) return;
      const failure = request.failure();
      networkIssues.push({ url: raw, error: failure ? failure.errorText : 'request failed' });
    });

    const lastURL = safeRead(policy.last_url_path);
    if (action.name === 'open_candidate') {
      await page.goto(action.url, { waitUntil: 'domcontentloaded', timeout: 15000 });
    } else if (lastURL) {
      await page.goto(lastURL, { waitUntil: 'domcontentloaded', timeout: 15000 }).catch(() => {});
    }

    const locator = () => action.selector ? page.locator(action.selector).first() : page.getByText(action.text, { exact: false }).first();
    if (action.name === 'wait') {
      await page.waitForTimeout(Math.max(100, Math.min(action.wait_ms || 1000, 5000)));
    } else if (action.name === 'click_navigation' || action.name === 'click_button') {
      await Promise.race([
        locator().click({ timeout: 5000 }),
        new Promise((_, reject) => setTimeout(() => reject(new Error('click timeout')), 5500))
      ]);
      await page.waitForLoadState('domcontentloaded', { timeout: 5000 }).catch(() => {});
    } else if (action.name === 'fill_input') {
      await locator().fill(action.value || '', { timeout: 5000 });
    } else if (action.name === 'submit_local_form') {
      await locator().press('Enter', { timeout: 5000 });
      await page.waitForLoadState('domcontentloaded', { timeout: 5000 }).catch(() => {});
    } else if (action.name === 'go_back') {
      await page.goBack({ waitUntil: 'domcontentloaded', timeout: 5000 }).catch(() => {});
    }
    const observation = await collect(page, true, '');
    await browser.close();
    process.stdout.write(JSON.stringify(observation));
  } catch (err) {
    try {
      if (browser) await browser.close();
    } catch (_) {}
    process.stdout.write(JSON.stringify({
      action: action.name,
      ok: false,
      console_errors: consoleErrors,
      page_errors: pageErrors,
      network_issues: networkIssues,
      blocked_requests: blockedRequests,
      error: err && err.message ? err.message : String(err)
    }));
  }
})();
`
