package redact

import "regexp"

var (
	agentEnvPattern               = regexp.MustCompile(`(?i)(--ae\s+[A-Za-z0-9_.-]+)=([^[:space:]"']+)`)
	secretAssignmentPattern       = regexp.MustCompile(`(?i)\b([A-Z0-9_.-]*(?:TOKEN|KEY|SECRET|PASSWORD|AUTH)[A-Z0-9_.-]*)=([^[:space:]"']+)`)
	secretKeyValuePattern         = regexp.MustCompile(`(?i)(["']?[A-Z0-9_.-]*(?:TOKEN|KEY|SECRET|PASSWORD|AUTH)[A-Z0-9_.-]*["']?\s*[:=]\s*)(["']?)([^"',}\s]+)(["']?)`)
	bearerPattern                 = regexp.MustCompile(`(?i)\b(Bearer\s+)[A-Za-z0-9._~+/=-]+`)
	skPattern                     = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}\b`)
	urlUserInfoPattern            = regexp.MustCompile(`(?i)((?:https?|ssh)://)[^/@[:space:]"']+@`)
	githubClassicTokenPattern     = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9_]{8,}\b`)
	githubFineGrainedTokenPattern = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{10,}\b`)
	awsAccessKeyPattern           = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{12,}\b`)
	sensitiveKeyPattern           = regexp.MustCompile(`(?i)(TOKEN|KEY|SECRET|PASSWORD|AUTH|CREDENTIAL)`)
)

func Text(text string) string {
	if text == "" {
		return ""
	}
	text = agentEnvPattern.ReplaceAllString(text, "$1=<redacted>")
	text = bearerPattern.ReplaceAllString(text, "$1<redacted>")
	text = skPattern.ReplaceAllString(text, "sk-<redacted>")
	text = githubFineGrainedTokenPattern.ReplaceAllString(text, "github_pat_<redacted>")
	text = githubClassicTokenPattern.ReplaceAllString(text, "gh_<redacted>")
	text = awsAccessKeyPattern.ReplaceAllString(text, "<redacted-aws-access-key>")
	text = urlUserInfoPattern.ReplaceAllString(text, "$1<redacted>@")
	text = secretAssignmentPattern.ReplaceAllString(text, "$1=<redacted>")
	text = secretKeyValuePattern.ReplaceAllString(text, "$1$2<redacted>$4")
	return text
}

func SensitiveKey(key string) bool {
	return sensitiveKeyPattern.MatchString(key)
}
