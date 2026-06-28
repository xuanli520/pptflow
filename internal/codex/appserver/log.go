package appserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

func formatAppServerRPCLogLine(message appServerRPCMessage) string {
	id := appServerRPCIDLog(message.ID)
	if len(message.ID) > 0 && message.Method == "" {
		if message.Error != nil {
			return fmt.Sprintf("JSON-RPC response id=%s error code=%d message=%q\n", id, message.Error.Code, truncateAppServerLogValue(message.Error.Message))
		}
		return fmt.Sprintf("JSON-RPC response id=%s result_%s\n", id, appServerJSONSummary(message.Result))
	}
	if len(message.ID) > 0 && message.Method != "" {
		return fmt.Sprintf("JSON-RPC server request id=%s method=%s params_%s\n", id, message.Method, appServerJSONSummary(message.Params))
	}
	if message.Method == "" {
		return fmt.Sprintf("JSON-RPC message params_%s\n", appServerJSONSummary(message.Params))
	}
	switch message.Method {
	case "item/agentMessage/delta":
		var params struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			starts, ends := staticReviewMarkerCounts(params.Delta)
			return fmt.Sprintf(
				"JSON-RPC notification item/agentMessage/delta turn=%s item=%s delta_bytes=%d delta_sha256=%s contract_starts=%d contract_ends=%d\n",
				compactAppServerLogID(params.TurnID),
				compactAppServerLogID(params.ItemID),
				len(params.Delta),
				shortAppServerLogHash(params.Delta),
				starts,
				ends,
			)
		}
	case "item/completed":
		var params struct {
			TurnID string        `json:"turnId"`
			Item   appServerItem `json:"item"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			starts, ends := staticReviewMarkerCounts(params.Item.Text)
			return fmt.Sprintf(
				"JSON-RPC notification item/completed turn=%s item=%s type=%s text_bytes=%d text_sha256=%s contract_starts=%d contract_ends=%d\n",
				compactAppServerLogID(params.TurnID),
				compactAppServerLogID(params.Item.ID),
				truncateAppServerLogValue(params.Item.Type),
				len(params.Item.Text),
				shortAppServerLogHash(params.Item.Text),
				starts,
				ends,
			)
		}
	case "turn/completed":
		var params struct {
			Turn struct {
				ID     string          `json:"id"`
				Items  []appServerItem `json:"items"`
				Status string          `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(message.Params, &params) == nil {
			if params.Turn.Error != nil && strings.TrimSpace(params.Turn.Error.Message) != "" {
				return fmt.Sprintf(
					"JSON-RPC notification turn/completed turn=%s status=%s items=%d error=%q\n",
					compactAppServerLogID(params.Turn.ID),
					truncateAppServerLogValue(params.Turn.Status),
					len(params.Turn.Items),
					truncateAppServerLogValue(params.Turn.Error.Message),
				)
			}
			return fmt.Sprintf(
				"JSON-RPC notification turn/completed turn=%s status=%s items=%d\n",
				compactAppServerLogID(params.Turn.ID),
				truncateAppServerLogValue(params.Turn.Status),
				len(params.Turn.Items),
			)
		}
	}
	return fmt.Sprintf("JSON-RPC notification %s params_%s\n", message.Method, appServerJSONSummary(message.Params))
}

func appServerRPCIDLog(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return compactAppServerLogID(text)
	}
	return truncateAppServerLogValue(string(raw))
}

func appServerJSONSummary(raw json.RawMessage) string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "null" {
		return "empty"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) == nil && object != nil {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 8 {
			keys = append(keys[:8], fmt.Sprintf("+%d", len(keys)-8))
		}
		return fmt.Sprintf("keys=%s bytes=%d sha256=%s", strings.Join(keys, ","), len(raw), shortAppServerLogHash(string(raw)))
	}
	return fmt.Sprintf("bytes=%d sha256=%s", len(raw), shortAppServerLogHash(string(raw)))
}

func compactAppServerLogID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	if len(value) <= 32 {
		return value
	}
	return value[:18] + "..." + value[len(value)-10:]
}

func truncateAppServerLogValue(value string) string {
	const limit = 512
	value = strings.ReplaceAll(value, "\r", "\\r")
	value = strings.ReplaceAll(value, "\n", "\\n")
	value = strings.ReplaceAll(value, "\t", "\\t")
	return truncateStringPrefix(value, limit)
}

func shortAppServerLogHash(value string) string {
	const length = 12
	sum := sha256Text(value)
	if len(sum) <= length {
		return sum
	}
	return sum[:length]
}

func prefixRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	for index := range value {
		if limit == 0 {
			return value[:index]
		}
		limit--
	}
	return value
}

func (s *appServerCodexReviewSession) appendLog(content string) {
	s.mu.Lock()
	path := s.req.LogPath
	s.mu.Unlock()
	if strings.TrimSpace(path) == "" {
		return
	}
	if err := appendText(path, content); err != nil {
		s.addWarning(newWarning(path, "append_text", false, err))
	}
}

func (s *appServerCodexReviewSession) addWarning(warning Warning) {
	if warning.OK() {
		return
	}
	s.mu.Lock()
	s.warnings = append(s.warnings, warning)
	s.mu.Unlock()
}

func commandString(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

const (
	staticReviewJSONStart = "<!-- pptflow:agent-json:start -->"
	staticReviewJSONEnd   = "<!-- pptflow:agent-json:end -->"
)

func newWarning(path, op string, required bool, err error) Warning {
	if err == nil {
		return Warning{}
	}
	return Warning{
		Path:       filepath.Clean(path),
		Op:         op,
		Error:      err.Error(),
		Required:   required,
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func writeText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func appendText(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(content)
	return err
}

func sha256Text(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func staticReviewMarkerCounts(value string) (int, int) {
	return strings.Count(value, staticReviewJSONStart), strings.Count(value, staticReviewJSONEnd)
}

func truncateStringPrefix(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}
