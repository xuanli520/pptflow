package pipeline

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/xuanli520/p2r_tui/internal/pipeline/model"
)

const (
	staticReviewSchemaVersion = "p2r.static_review.v1"
	staticReviewJSONStart     = "<!-- p2r:static-review-json:start -->"
	staticReviewJSONEnd       = "<!-- p2r:static-review-json:end -->"
)

type staticReviewReportSchema struct {
	SchemaVersion string                      `json:"schema_version"`
	Stage         string                      `json:"stage"`
	Findings      []staticReviewFindingSchema `json:"findings"`
}

type staticReviewFindingSchema struct {
	Severity     string     `json:"severity"`
	Title        string     `json:"title"`
	Rule         string     `json:"rule"`
	Evidence     reviewText `json:"evidence"`
	Impact       string     `json:"impact"`
	DoneCriteria string     `json:"done_criteria,omitempty"`
	MinimumFix   string     `json:"minimum_fix"`
}

type reviewText string

func (v *reviewText) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*v = reviewText(strings.TrimSpace(value))
		return nil
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("must be a string or string array")
	}
	var cleaned []string
	for _, item := range values {
		item = strings.TrimSpace(item)
		if item != "" {
			cleaned = append(cleaned, item)
		}
	}
	*v = reviewText(strings.Join(cleaned, "\n"))
	return nil
}

func assignFindingIDs(stage string, findings []model.Finding) []model.Finding {
	counts := map[string]int{}
	for i := range findings {
		findings[i].Stage = stage
		short := severityShort(findings[i].Severity)
		counts[short]++
		findings[i].ID = fmt.Sprintf("P2R-%s-%s-%03d", stage, short, counts[short])
	}
	return findings
}

func assignMissingFindingIDs(stage string, findings []model.Finding) []model.Finding {
	counts := map[string]int{}
	for _, finding := range findings {
		if finding.Stage == stage && finding.ID != "" {
			short := severityShort(finding.Severity)
			counts[short]++
		}
	}
	for i := range findings {
		if findings[i].Stage == "" {
			findings[i].Stage = stage
		}
		if findings[i].ID != "" {
			continue
		}
		short := severityShort(findings[i].Severity)
		counts[short]++
		findings[i].ID = fmt.Sprintf("P2R-%s-%s-%03d", findings[i].Stage, short, counts[short])
	}
	return findings
}

func severityShort(severity string) string {
	switch strings.ToLower(severity) {
	case "blocker":
		return "BLK"
	case "high":
		return "HIGH"
	case "medium":
		return "MED"
	default:
		return "LOW"
	}
}

func extractFindingsFromReport(stage, report, sourcePath string) []model.Finding {
	findings, err := staticReviewFindingsFromReport(stage, report, sourcePath)
	if err != nil {
		return nil
	}
	return findings
}

func staticReviewFindingsFromReport(stage, report, sourcePath string) ([]model.Finding, error) {
	payload, err := extractStaticReviewJSON(report)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("invalid static review JSON: %w", err)
	}
	for _, key := range []string{"schema_version", "stage", "findings"} {
		if _, ok := raw[key]; !ok {
			return nil, fmt.Errorf("static review JSON missing required field %q", key)
		}
	}
	var rawFindings []json.RawMessage
	if err := json.Unmarshal(raw["findings"], &rawFindings); err != nil {
		return nil, fmt.Errorf("static review JSON field findings must be an array: %w", err)
	}
	var schema staticReviewReportSchema
	if err := json.Unmarshal([]byte(payload), &schema); err != nil {
		return nil, fmt.Errorf("static review JSON does not match schema: %w", err)
	}
	if strings.TrimSpace(schema.SchemaVersion) != staticReviewSchemaVersion {
		return nil, fmt.Errorf("static review JSON schema_version = %q, want %q", schema.SchemaVersion, staticReviewSchemaVersion)
	}
	if strings.ToUpper(strings.TrimSpace(schema.Stage)) != strings.ToUpper(strings.TrimSpace(stage)) {
		return nil, fmt.Errorf("static review JSON stage = %q, want %q", schema.Stage, stage)
	}
	var findings []model.Finding
	seen := map[string]bool{}
	for index, item := range schema.Findings {
		severity, err := normalizeReviewSeverity(item.Severity)
		if err != nil {
			return nil, fmt.Errorf("finding %d: %w", index+1, err)
		}
		title := strings.TrimSpace(item.Title)
		if title == "" {
			return nil, fmt.Errorf("finding %d: title is required", index+1)
		}
		rule := strings.TrimSpace(item.Rule)
		evidence := strings.TrimSpace(string(item.Evidence))
		impact := strings.TrimSpace(item.Impact)
		minimumFix := strings.TrimSpace(item.MinimumFix)
		for field, value := range map[string]string{
			"rule":        rule,
			"evidence":    evidence,
			"impact":      impact,
			"minimum_fix": minimumFix,
		} {
			if value == "" {
				return nil, fmt.Errorf("finding %d: %s is required", index+1, field)
			}
		}
		key := strings.Join([]string{severity, title, rule, evidence}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, model.Finding{
			Stage:        stage,
			Severity:     severity,
			Title:        title,
			Rule:         rule,
			Evidence:     evidence,
			Impact:       impact,
			DoneCriteria: strings.TrimSpace(item.DoneCriteria),
			MinimumFix:   minimumFix,
			SourcePath:   sourcePath,
		})
	}
	return findings, nil
}

func normalizeStaticReviewReport(report string) (string, error) {
	report = strings.TrimSpace(report)
	if report == "" {
		return "", fmt.Errorf("static review report is empty")
	}
	blocks, err := staticReviewJSONBlocks(report)
	if err != nil {
		return "", err
	}
	contract := staticReviewContractForBlocks(report, blocks)
	body := staticReviewReportWithoutBlocks(report, blocks)
	body, err = trimStaticReviewReportPreamble(body)
	if err != nil {
		return "", err
	}
	return body + "\n\n" + contract, nil
}

func extractStaticReviewJSON(report string) (string, error) {
	_, _, payload, err := staticReviewJSONBlock(report)
	return payload, err
}

func staticReviewJSONBlock(report string) (int, int, string, error) {
	blocks, err := staticReviewJSONBlocks(report)
	if err != nil {
		return 0, 0, "", err
	}
	if len(blocks) > 1 {
		return 0, 0, "", fmt.Errorf("static review report contains multiple JSON contract blocks")
	}
	block := blocks[0]
	return block.start, block.blockEnd, block.payload, nil
}

type staticReviewJSONBlockRange struct {
	start    int
	blockEnd int
	payload  string
}

func staticReviewJSONBlocks(report string) ([]staticReviewJSONBlockRange, error) {
	var blocks []staticReviewJSONBlockRange
	offset := 0
	for {
		startOffset := strings.Index(report[offset:], staticReviewJSONStart)
		if startOffset < 0 {
			break
		}
		start := offset + startOffset
		afterStart := start + len(staticReviewJSONStart)
		endOffset := strings.Index(report[afterStart:], staticReviewJSONEnd)
		if endOffset < 0 {
			return nil, fmt.Errorf("static review report missing %s marker", staticReviewJSONEnd)
		}
		end := afterStart + endOffset
		blockEnd := end + len(staticReviewJSONEnd)
		payload := trimMarkdownFence(strings.TrimSpace(report[afterStart:end]))
		if payload == "" {
			return nil, fmt.Errorf("static review JSON contract is empty")
		}
		blocks = append(blocks, staticReviewJSONBlockRange{start: start, blockEnd: blockEnd, payload: payload})
		offset = blockEnd
	}
	if len(blocks) == 0 {
		if strings.Contains(report, staticReviewJSONEnd) {
			return nil, fmt.Errorf("static review report missing %s marker", staticReviewJSONStart)
		}
		return nil, fmt.Errorf("static review report missing %s marker", staticReviewJSONStart)
	}
	if strings.Count(report, staticReviewJSONEnd) > len(blocks) {
		return nil, fmt.Errorf("static review report contains extra JSON contract end markers")
	}
	return blocks, nil
}

func staticReviewReportWithoutBlocks(report string, blocks []staticReviewJSONBlockRange) string {
	var builder strings.Builder
	cursor := 0
	for _, block := range blocks {
		builder.WriteString(strings.TrimSpace(report[cursor:block.start]))
		builder.WriteString("\n\n")
		cursor = block.blockEnd
	}
	builder.WriteString(strings.TrimSpace(report[cursor:]))
	return strings.TrimSpace(builder.String())
}

func staticReviewContractForBlocks(report string, blocks []staticReviewJSONBlockRange) string {
	if len(blocks) > 1 {
		if contract, ok := combinedStaticReviewContract(blocks); ok {
			return contract
		}
	}
	contractBlock := blocks[len(blocks)-1]
	return strings.TrimSpace(report[contractBlock.start:contractBlock.blockEnd])
}

func combinedStaticReviewContract(blocks []staticReviewJSONBlockRange) (string, bool) {
	var combined staticReviewReportSchema
	combined.SchemaVersion = staticReviewSchemaVersion
	combined.Findings = []staticReviewFindingSchema{}
	seen := map[string]bool{}
	for _, block := range blocks {
		var schema staticReviewReportSchema
		if err := json.Unmarshal([]byte(block.payload), &schema); err != nil {
			return "", false
		}
		if strings.TrimSpace(schema.SchemaVersion) != staticReviewSchemaVersion {
			return "", false
		}
		if strings.TrimSpace(schema.Stage) == "" {
			return "", false
		}
		if combined.Stage == "" {
			combined.Stage = strings.TrimSpace(schema.Stage)
		}
		if !strings.EqualFold(strings.TrimSpace(schema.Stage), combined.Stage) {
			return "", false
		}
		for _, finding := range schema.Findings {
			key := strings.Join([]string{
				strings.TrimSpace(finding.Severity),
				strings.TrimSpace(finding.Title),
				strings.TrimSpace(finding.Rule),
				strings.TrimSpace(string(finding.Evidence)),
			}, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			combined.Findings = append(combined.Findings, finding)
		}
	}
	content, err := json.MarshalIndent(combined, "", "  ")
	if err != nil {
		return "", false
	}
	return staticReviewJSONStart + "\n" + string(content) + "\n" + staticReviewJSONEnd, true
}

func staticReviewMarkerCounts(value string) (int, int) {
	return strings.Count(value, staticReviewJSONStart), strings.Count(value, staticReviewJSONEnd)
}

func truncateStaticReviewReport(report string, limit int) string {
	if limit <= 0 || len(report) <= limit {
		return report
	}
	start, blockEnd, _, err := staticReviewJSONBlock(report)
	if err != nil {
		return truncateString(report, limit)
	}
	contract := strings.TrimSpace(report[start:blockEnd])
	body := strings.TrimSpace(strings.Join([]string{
		strings.TrimSpace(report[:start]),
		strings.TrimSpace(report[blockEnd:]),
	}, "\n\n"))
	if body == "" {
		return contract
	}
	const (
		truncationMarker = "\n\n[truncated]"
		reportSeparator  = "\n\n"
	)
	bodyLimit := limit - len(contract) - len(truncationMarker) - len(reportSeparator)
	if bodyLimit <= 0 {
		return contract
	}
	if len(body) > bodyLimit {
		body = strings.TrimSpace(truncateStringPrefix(body, bodyLimit)) + truncationMarker
	}
	return strings.TrimSpace(body) + reportSeparator + contract
}

func trimStaticReviewReportPreamble(body string) (string, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("static review report body is empty")
	}
	start := staticReviewReportStartIndex(body)
	if start < 0 {
		return "", fmt.Errorf("static review report must begin with a markdown heading or numbered section before the JSON contract")
	}
	return strings.TrimSpace(body[start:]), nil
}

func staticReviewReportStartIndex(body string) int {
	offset := 0
	for offset <= len(body) {
		next := strings.IndexByte(body[offset:], '\n')
		lineEnd := len(body)
		if next >= 0 {
			lineEnd = offset + next
		}
		line := body[offset:lineEnd]
		trimmed := strings.TrimSpace(line)
		if isStaticReviewReportStartLine(trimmed) {
			return offset + len(line) - len(strings.TrimLeft(line, " \t\r"))
		}
		if next < 0 {
			break
		}
		offset = lineEnd + 1
	}
	return -1
}

func isStaticReviewReportStartLine(line string) bool {
	if strings.HasPrefix(line, "1. ") {
		return true
	}
	if !strings.HasPrefix(line, "#") {
		return false
	}
	hashes := 0
	for hashes < len(line) && line[hashes] == '#' {
		hashes++
	}
	return hashes >= 1 && hashes <= 6 && hashes < len(line) && (line[hashes] == ' ' || line[hashes] == '\t')
}

func trimMarkdownFence(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) < 3 {
		return value
	}
	first := strings.TrimSpace(lines[0])
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(first, "```") || !strings.HasPrefix(last, "```") {
		return value
	}
	return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
}

func normalizeReviewSeverity(severity string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "blocker":
		return "Blocker", nil
	case "high":
		return "High", nil
	case "medium":
		return "Medium", nil
	case "low":
		return "Low", nil
	default:
		return "", fmt.Errorf("severity must be one of Blocker, High, Medium, Low")
	}
}

func countSeverity(findings []model.Finding, severity string) int {
	count := 0
	for _, finding := range findings {
		if finding.Severity == severity {
			count++
		}
	}
	return count
}

func highestRisk(findings []model.Finding) model.Finding {
	if len(findings) == 0 {
		return model.Finding{}
	}
	sortFindings(findings)
	return findings[0]
}

func sortFindings(findings []model.Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri := severityRank(findings[i].Severity)
		rj := severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return findings[i].ID < findings[j].ID
	})
}

func severityRank(severity string) int {
	switch strings.ToLower(severity) {
	case "blocker":
		return 0
	case "high":
		return 1
	case "medium":
		return 2
	default:
		return 3
	}
}
