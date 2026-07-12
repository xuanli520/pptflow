package app

import (
	"os"
	"strings"
)

var claudeCredentialKeys = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// ExtractRuntimeOptions keeps values that are intentionally absent from a
// persisted runner snapshot or are expected to follow the current process.
func ExtractRuntimeOptions(opts RunnerOptions) RunnerOptions {
	return RunnerOptions{
		GitHubToken:       opts.GitHubToken,
		HarborAgentEnv:    append([]string(nil), opts.HarborAgentEnv...),
		QwenHarborBaseURL: opts.QwenHarborBaseURL,
		OpusHarborBaseURL: opts.OpusHarborBaseURL,
		HarborExec:        opts.HarborExec,
		VerifyExec:        opts.VerifyExec,
		Agent:             opts.Agent,
	}
}

// RuntimeOptionsFromEnvironment captures supported process-level credentials
// and routes. Values remain in process memory; HarborAgentEnv contains only a
// safe KEY=${KEY} reference.
func RuntimeOptionsFromEnvironment() RunnerOptions {
	var runtimeOptions RunnerOptions
	for _, key := range claudeCredentialKeys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			runtimeOptions.HarborAgentEnv = []string{key + "=${" + key + "}"}
			break
		}
	}
	runtimeOptions.GitHubToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	fallback := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	runtimeOptions.QwenHarborBaseURL = environmentValue("QWEN_HARBOR_BASE_URL", fallback)
	runtimeOptions.OpusHarborBaseURL = environmentValue("OPUS_HARBOR_BASE_URL", fallback)
	return runtimeOptions
}

// HydrateRuntimeOptions makes Runner construction safe even when a caller did
// not pass through the CLI defaults. Explicit non-empty options remain in
// control; unresolved historical Claude credential templates are discarded.
func HydrateRuntimeOptions(opts RunnerOptions) RunnerOptions {
	opts.HarborAgentEnv = pruneUnresolvedClaudeCredentials(opts.HarborAgentEnv)
	runtimeOptions := RuntimeOptionsFromEnvironment()
	if !hasClaudeCredential(opts.HarborAgentEnv) {
		opts.HarborAgentEnv = mergeAgentEnvironment(opts.HarborAgentEnv, runtimeOptions.HarborAgentEnv)
	}
	if strings.TrimSpace(opts.GitHubToken) == "" {
		opts.GitHubToken = runtimeOptions.GitHubToken
	}
	if strings.TrimSpace(opts.QwenHarborBaseURL) == "" {
		opts.QwenHarborBaseURL = runtimeOptions.QwenHarborBaseURL
	}
	if strings.TrimSpace(opts.OpusHarborBaseURL) == "" {
		opts.OpusHarborBaseURL = runtimeOptions.OpusHarborBaseURL
	}
	return opts
}

// MergeRuntimeOptions applies current process/session state over a historical
// snapshot. Current runtime values deliberately win over stale persisted
// routes and credential references.
func MergeRuntimeOptions(opts, runtimeOptions RunnerOptions) RunnerOptions {
	opts.HarborAgentEnv = mergeAgentEnvironment(pruneUnresolvedClaudeCredentials(opts.HarborAgentEnv), runtimeOptions.HarborAgentEnv)
	if strings.TrimSpace(runtimeOptions.GitHubToken) != "" {
		opts.GitHubToken = runtimeOptions.GitHubToken
	}
	if strings.TrimSpace(runtimeOptions.QwenHarborBaseURL) != "" {
		opts.QwenHarborBaseURL = runtimeOptions.QwenHarborBaseURL
	}
	if strings.TrimSpace(runtimeOptions.OpusHarborBaseURL) != "" {
		opts.OpusHarborBaseURL = runtimeOptions.OpusHarborBaseURL
	}
	if runtimeOptions.HarborExec != nil {
		opts.HarborExec = runtimeOptions.HarborExec
	}
	if runtimeOptions.VerifyExec != nil {
		opts.VerifyExec = runtimeOptions.VerifyExec
	}
	if runtimeOptions.Agent != nil {
		opts.Agent = runtimeOptions.Agent
	}
	return HydrateRuntimeOptions(opts)
}

func mergeAgentEnvironment(base, current []string) []string {
	out := append([]string(nil), base...)
	for _, item := range current {
		key, value, ok := strings.Cut(item, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.TrimSpace(value) == "" {
			continue
		}
		if isClaudeCredentialKey(key) && !credentialAssignmentResolved(value) {
			continue
		}
		out = upsertEnv(out, key, strings.TrimSpace(value))
	}
	return out
}

func pruneUnresolvedClaudeCredentials(values []string) []string {
	out := make([]string, 0, len(values))
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if !ok || !isClaudeCredentialKey(key) || credentialAssignmentResolved(value) {
			out = append(out, item)
		}
	}
	return out
}

func credentialAssignmentResolved(value string) bool {
	value = strings.TrimSpace(value)
	if envName, templated := envTemplateName(value); templated {
		return strings.TrimSpace(os.Getenv(envName)) != ""
	}
	return value != ""
}

func isClaudeCredentialKey(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	for _, allowed := range claudeCredentialKeys {
		if key == allowed {
			return true
		}
	}
	return false
}

func environmentValue(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
