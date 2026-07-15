package appserver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type aggregatedDeltaLog struct {
	turnID string
	itemID string
	text   string
}

func formatAggregatedDeltaLogLine(log aggregatedDeltaLog) string {
	return fmt.Sprintf(
		"JSON-RPC notification item/agentMessage/delta aggregated turn=%s item=%s total_bytes=%d delta_sha256=%s text_prefix=%q\n",
		compactAppServerLogID(log.turnID),
		compactAppServerLogID(log.itemID),
		len(log.text),
		shortAppServerLogHash(log.text),
		truncateAppServerLogValue(prefixRunes(log.text, 10)),
	)
}

func (s *appServerSession) aggregatedDeltaLogForItem(itemID string) (aggregatedDeltaLog, bool) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return aggregatedDeltaLog{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text := s.deltas[itemID]
	if text == "" {
		return aggregatedDeltaLog{}, false
	}
	if s.deltaLogged == nil {
		s.deltaLogged = map[string]bool{}
	}
	if s.deltaLogged[itemID] {
		return aggregatedDeltaLog{}, false
	}
	s.deltaLogged[itemID] = true
	return aggregatedDeltaLog{turnID: s.turnID, itemID: itemID, text: text}, true
}

func (s *appServerSession) logAggregatedDelta(itemID string) {
	log, ok := s.aggregatedDeltaLogForItem(itemID)
	if !ok {
		return
	}
	s.appendLog(formatAggregatedDeltaLogLine(log))
}

func (s *appServerSession) remainingAggregatedDeltaLogs() []aggregatedDeltaLog {
	s.mu.Lock()
	if s.deltaLogged == nil {
		s.deltaLogged = map[string]bool{}
	}
	logs := make([]aggregatedDeltaLog, 0, len(s.deltas))
	for itemID, text := range s.deltas {
		if text == "" || s.deltaLogged[itemID] {
			continue
		}
		s.deltaLogged[itemID] = true
		logs = append(logs, aggregatedDeltaLog{turnID: s.turnID, itemID: itemID, text: text})
	}
	s.mu.Unlock()
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].itemID < logs[j].itemID
	})
	return logs
}

func (s *appServerSession) logRemainingAggregatedDeltas() {
	for _, log := range s.remainingAggregatedDeltaLogs() {
		s.appendLog(formatAggregatedDeltaLogLine(log))
	}
}

const appServerDeltaPreviewMaxBytes = 64 * 1024

func appendDeltaPreview(current, delta string, truncated bool) (string, bool) {
	return deltaPreviewText(current+delta, truncated)
}

func deltaPreviewText(text string, truncated bool) (string, bool) {
	if len(text) <= appServerDeltaPreviewMaxBytes {
		return strings.ToValidUTF8(text, ""), truncated
	}
	return utf8SafeSuffix(text, appServerDeltaPreviewMaxBytes), true
}

func utf8SafeSuffix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return strings.ToValidUTF8(value, "")
	}
	start := len(value) - limit
	for start < len(value) && !utf8.RuneStart(value[start]) {
		start++
	}
	return strings.ToValidUTF8(value[start:], "")
}

func utf8SafePrefix(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return strings.ToValidUTF8(value, "")
	}
	return strings.ToValidUTF8(truncateStringPrefix(value, limit), "")
}

func appendOutputBounded(current, addition string, limit int) (string, string, bool) {
	if addition == "" {
		return current, "", false
	}
	if limit <= 0 {
		return current + addition, addition, false
	}
	remaining := limit - len(current)
	if remaining <= 0 {
		return current, "", true
	}
	if len(addition) <= remaining {
		return current + addition, addition, false
	}
	appended := utf8SafePrefix(addition, remaining)
	return current + appended, appended, true
}

func (s *appServerSession) emitDeltaUpdate(update Update, ok bool) {
	if !ok {
		return
	}
	s.mu.Lock()
	onDelta := s.req.OnDelta
	s.mu.Unlock()
	if onDelta != nil {
		onDelta(update)
	}
}

func (s *appServerSession) recordItemActivity(turnID, itemID, itemType string, raw json.RawMessage, done bool) {
	itemType = strings.TrimSpace(itemType)
	if itemType == "" || itemType == "userMessage" {
		return
	}
	s.mu.Lock()
	if s.completed || s.turnDone == nil || (s.turnID != "" && turnID != s.turnID) || s.agentPreviewStarted {
		s.mu.Unlock()
		return
	}
	line := codexActivityLine(itemType, raw, done)
	if line == "" {
		s.mu.Unlock()
		return
	}
	if itemID == "" {
		itemID = "__codex_activity__"
	}
	preview, truncated := deltaPreviewText(s.activityPreview+line+"\n", s.activityTruncated)
	s.activityPreview = preview
	s.activityTruncated = truncated
	update := Update{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Delta:     line + "\n",
		Text:      preview,
		Truncated: truncated,
	}
	s.mu.Unlock()
	s.emitDeltaUpdate(update, true)
}

func codexActivityLine(itemType string, raw json.RawMessage, done bool) string {
	detail := appServerActivityDetail(raw)
	switch itemType {
	case "agentMessage":
		if done {
			return "Codex 已完成回复。"
		}
		return "Codex 正在生成回复..."
	case "reasoning":
		if done {
			return "Codex 完成一段分析。"
		}
		return "Codex 正在分析..."
	case "commandExecution":
		if done {
			if detail != "" {
				return "Codex 完成命令: " + detail
			}
			return "Codex 完成命令执行。"
		}
		if detail != "" {
			return "Codex 正在执行命令: " + detail
		}
		return "Codex 正在执行命令..."
	default:
		if done {
			return "Codex 完成事件: " + truncateAppServerLogValue(itemType)
		}
		return "Codex 事件: " + truncateAppServerLogValue(itemType)
	}
}

func appServerActivityDetail(raw json.RawMessage) string {
	for _, path := range [][]string{
		{"item", "command"},
		{"item", "cmd"},
		{"item", "name"},
		{"item", "title"},
		{"command"},
		{"cmd"},
		{"name"},
		{"title"},
	} {
		if value := appServerStringAtPath(raw, path...); value != "" {
			return truncateAppServerLogValue(value)
		}
	}
	return ""
}

func appServerStringAtPath(raw json.RawMessage, path ...string) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = object[key]
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				parts = append(parts, strings.TrimSpace(text))
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func (s *appServerSession) recordDelta(turnID, itemID, delta string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	if s.completed || s.turnDone == nil || (s.turnID != "" && turnID != s.turnID) {
		s.mu.Unlock()
		return
	}
	if s.deltas == nil {
		s.deltas = map[string]string{}
	}
	if s.deltaPreview == nil {
		s.deltaPreview = map[string]string{}
	}
	if s.deltaPreviewTruncated == nil {
		s.deltaPreviewTruncated = map[string]bool{}
	}
	if s.itemDone == nil {
		s.itemDone = map[string]bool{}
	}
	if _, ok := s.deltas[itemID]; !ok {
		if _, hasItem := s.items[itemID]; !hasItem {
			s.itemOrder = append(s.itemOrder, itemID)
		}
	}
	limit := s.req.MaxOutputBytes
	next, storedDelta, outputTruncated := appendOutputBounded(s.deltas[itemID], delta, limit)
	s.deltas[itemID] = next
	s.agentPreviewStarted = true
	if outputTruncated {
		s.deltaPreviewTruncated[itemID] = true
	}
	if s.itemDone[itemID] {
		s.mu.Unlock()
		return
	}
	preview, truncated := appendDeltaPreview(s.deltaPreview[itemID], storedDelta, s.deltaPreviewTruncated[itemID])
	s.deltaPreview[itemID] = preview
	s.deltaPreviewTruncated[itemID] = truncated
	update := Update{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Delta:     storedDelta,
		Text:      preview,
		Truncated: truncated,
	}
	emit := storedDelta != "" || outputTruncated
	s.mu.Unlock()
	s.emitDeltaUpdate(update, emit)
}

func (s *appServerSession) recordCompletedItem(turnID, itemID, text string) {
	if strings.TrimSpace(itemID) == "" {
		return
	}
	s.mu.Lock()
	if s.completed || s.turnDone == nil || (s.turnID != "" && turnID != s.turnID) {
		s.mu.Unlock()
		return
	}
	if s.items == nil {
		s.items = map[string]string{}
	}
	if s.deltas == nil {
		s.deltas = map[string]string{}
	}
	if s.deltaPreview == nil {
		s.deltaPreview = map[string]string{}
	}
	if s.deltaPreviewTruncated == nil {
		s.deltaPreviewTruncated = map[string]bool{}
	}
	if s.itemDone == nil {
		s.itemDone = map[string]bool{}
	}
	if _, ok := s.items[itemID]; !ok && s.deltas[itemID] == "" {
		s.itemOrder = append(s.itemOrder, itemID)
	}
	storedText, _, outputTruncated := appendOutputBounded("", text, s.req.MaxOutputBytes)
	s.items[itemID] = storedText
	hadPreview := s.deltaPreview[itemID] != "" || s.deltas[itemID] != ""
	s.itemDone[itemID] = true
	if storedText != "" {
		s.agentPreviewStarted = true
	}
	preview := s.deltaPreview[itemID]
	truncated := s.deltaPreviewTruncated[itemID] || outputTruncated
	if storedText != "" {
		preview, truncated = deltaPreviewText(storedText, truncated)
	}
	update := Update{
		TurnID:    firstNonEmpty(turnID, s.turnID),
		ItemID:    itemID,
		Text:      preview,
		Done:      true,
		Truncated: truncated,
	}
	s.mu.Unlock()
	s.emitDeltaUpdate(update, hadPreview || storedText != "" || outputTruncated)
}

func (s *appServerSession) finalReportLocked() string {
	if len(s.itemOrder) == 0 {
		for id := range s.items {
			s.itemOrder = append(s.itemOrder, id)
		}
		for id := range s.deltas {
			if _, ok := s.items[id]; !ok {
				s.itemOrder = append(s.itemOrder, id)
			}
		}
		sort.Strings(s.itemOrder)
	}
	var parts []string
	var report string
	limit := s.req.MaxOutputBytes
	for _, id := range s.itemOrder {
		if text := strings.TrimSpace(s.items[id]); text != "" {
			parts = append(parts, text)
			continue
		}
		if text := strings.TrimSpace(s.deltas[id]); text != "" {
			parts = append(parts, text)
		}
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			continue
		}
		addition := part
		if report != "" {
			addition = "\n\n" + addition
		}
		next, _, truncated := appendOutputBounded(report, addition, limit)
		report = next
		if truncated {
			break
		}
	}
	return strings.TrimSpace(report)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
