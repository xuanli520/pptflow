package harborrun

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/commandlog"
)

const apiRetryProgressInterval = time.Second

var harborRetryLinePattern = regexp.MustCompile(`^Trial ([^ ]+) failed with exception ([A-Za-z0-9_.-]+)\. Retrying in `)

type tailedFile struct {
	offset    int64
	remainder string
	prefix    string
}

type apiRetryProgress struct {
	trial      string
	attempt    int
	maxRetries int
	delayMS    int64
	status     string
	id         string
}

type retryEvidenceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type retryEvidenceEntry struct {
	Trial          string              `json:"trial"`
	Retry          int                 `json:"retry"`
	ExceptionClass string              `json:"exception_class"`
	Files          []retryEvidenceFile `json:"files"`
	CreatedAt      time.Time           `json:"created_at"`
}

type retryEvidenceManifest struct {
	SchemaVersion string               `json:"schema_version"`
	Entries       []retryEvidenceEntry `json:"entries"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type jobMonitor struct {
	jobsDir       string
	outputDir     string
	progress      func(line, source string)
	lastStats     string
	lastJobLog    string
	tails         map[string]*tailedFile
	retryCounts   map[string]int
	seenAPIRetry  map[string]struct{}
	pendingRetry  map[string]apiRetryProgress
	lastRetryEmit map[string]time.Time
	manifest      retryEvidenceManifest
}

func newJobMonitor(jobsDir, outputDir string, progress func(line, source string)) *jobMonitor {
	return &jobMonitor{
		jobsDir:       jobsDir,
		outputDir:     outputDir,
		progress:      progress,
		tails:         map[string]*tailedFile{},
		retryCounts:   map[string]int{},
		seenAPIRetry:  map[string]struct{}{},
		pendingRetry:  map[string]apiRetryProgress{},
		lastRetryEmit: map[string]time.Time{},
		manifest: retryEvidenceManifest{
			SchemaVersion: "harbor.retry_evidence.v1",
		},
	}
}

func (m *jobMonitor) poll(flush bool) {
	if state, ok := readJobProgress(m.jobsDir); ok {
		message := fmt.Sprintf("trials total=%d completed=%d errored=%d running=%d pending=%d cancelled=%d retries=%d", state.Total, state.Completed, state.Errored, state.Running, state.Pending, state.Cancelled, state.Retries)
		if message != m.lastStats {
			m.lastStats = message
			m.progress(commandlog.RedactText(message), "harbor-result")
		}
	}
	if line, ok := readLatestJobLogLine(m.jobsDir); ok && line != m.lastJobLog {
		m.lastJobLog = line
		m.progress(commandlog.RedactText(compactDiagnostic(line, 600)), "harbor-job")
	}
	m.scanJobLogs()
	m.scanAgentLogs()
	m.emitPendingAPIRetries(time.Now(), flush)
}

func (m *jobMonitor) scanJobLogs() {
	for _, path := range findFilesNamed(m.jobsDir, "job.log") {
		m.readNewLines(path, func(line string) {
			match := harborRetryLinePattern.FindStringSubmatch(strings.TrimSpace(line))
			if len(match) != 3 {
				return
			}
			trial, exceptionClass := match[1], match[2]
			m.retryCounts[trial]++
			if err := m.snapshotRetryEvidence(filepath.Dir(path), trial, exceptionClass, m.retryCounts[trial]); err != nil {
				m.progress("retry evidence snapshot failed for trial="+safeProgressValue(trial)+": "+compactDiagnostic(commandlog.RedactText(err.Error()), 300), "factory")
				return
			}
			m.progress(fmt.Sprintf("trial=%s retry=%d evidence_snapshotted", safeProgressValue(trial), m.retryCounts[trial]), "harbor-retry")
		})
	}
}

func (m *jobMonitor) scanAgentLogs() {
	for _, path := range findFilesNamed(m.jobsDir, "claude-code.txt") {
		trial := filepath.Base(filepath.Dir(filepath.Dir(path)))
		m.readNewLines(path, func(line string) {
			event, ok := parseAPIRetryProgress(line, trial)
			if !ok {
				return
			}
			key := path + "\x00" + event.id
			if event.id != "" {
				if _, exists := m.seenAPIRetry[key]; exists {
					return
				}
				m.seenAPIRetry[key] = struct{}{}
			}
			m.pendingRetry[trial] = event
		})
	}
}

func (m *jobMonitor) readNewLines(path string, consume func(string)) {
	state := m.tails[path]
	if state == nil {
		state = &tailedFile{}
		m.tails[path] = state
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	prefixSize := int64(256)
	if info.Size() < prefixSize {
		prefixSize = info.Size()
	}
	prefixRaw := make([]byte, prefixSize)
	if prefixSize > 0 {
		if _, err := io.ReadFull(file, prefixRaw); err != nil {
			return
		}
	}
	currentPrefix := string(prefixRaw)
	prefixChanged := state.prefix != "" && !strings.HasPrefix(currentPrefix, state.prefix)
	if info.Size() < state.offset || prefixChanged {
		state.offset = 0
		state.remainder = ""
		state.prefix = ""
	}
	if state.prefix == "" || len(currentPrefix) > len(state.prefix) {
		state.prefix = currentPrefix
	}
	if _, err := file.Seek(state.offset, 0); err != nil {
		return
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return
	}
	state.offset += int64(len(raw))
	text := state.remainder + string(raw)
	lines := strings.Split(text, "\n")
	state.remainder = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		consume(line)
	}
}

func (m *jobMonitor) emitPendingAPIRetries(now time.Time, flush bool) {
	trials := make([]string, 0, len(m.pendingRetry))
	for trial := range m.pendingRetry {
		trials = append(trials, trial)
	}
	sort.Strings(trials)
	for _, trial := range trials {
		if !flush && now.Sub(m.lastRetryEmit[trial]) < apiRetryProgressInterval {
			continue
		}
		event := m.pendingRetry[trial]
		m.progress(fmt.Sprintf("trial=%s api_retry attempt=%d/%d delay_ms=%d status=%s", safeProgressValue(event.trial), event.attempt, event.maxRetries, event.delayMS, safeProgressValue(event.status)), "harbor-api-retry")
		m.lastRetryEmit[trial] = now
		delete(m.pendingRetry, trial)
	}
}

func parseAPIRetryProgress(line, trial string) (apiRetryProgress, bool) {
	var payload struct {
		Type         string          `json:"type"`
		Subtype      string          `json:"subtype"`
		Attempt      int             `json:"attempt"`
		MaxRetries   int             `json:"max_retries"`
		RetryDelayMS float64         `json:"retry_delay_ms"`
		ErrorStatus  json.RawMessage `json:"error_status"`
		UUID         string          `json:"uuid"`
	}
	if json.Unmarshal([]byte(line), &payload) != nil || payload.Type != "system" || payload.Subtype != "api_retry" {
		return apiRetryProgress{}, false
	}
	return apiRetryProgress{
		trial:      trial,
		attempt:    payload.Attempt,
		maxRetries: payload.MaxRetries,
		delayMS:    int64(payload.RetryDelayMS + 0.5),
		status:     apiRetryStatus(payload.ErrorStatus),
		id:         payload.UUID,
	}, true
}

func apiRetryStatus(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "unknown"
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return safeProgressValue(value)
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return strconv.FormatInt(int64(number), 10)
	}
	return "unknown"
}

func safeProgressValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	var out strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._-", r) {
			out.WriteRune(r)
		}
		if out.Len() >= 80 {
			break
		}
	}
	if out.Len() == 0 {
		return "unknown"
	}
	return out.String()
}

func findFilesNamed(root, name string) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == name {
			paths = append(paths, path)
		}
		return nil
	})
	sort.Strings(paths)
	return paths
}

func (m *jobMonitor) snapshotRetryEvidence(jobDir, trial, exceptionClass string, retry int) error {
	trialDir := filepath.Join(jobDir, trial)
	info, err := os.Stat(trialDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("trial directory not found")
	}
	destination := filepath.Join(m.outputDir, "retry_evidence", safePathName(trial), fmt.Sprintf("retry-%02d", retry))
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	candidates := []string{
		"result.json",
		"exception.txt",
		"trial.log",
		filepath.Join("agent", "claude-code.txt"),
		filepath.Join("agent", "trajectory.json"),
		filepath.Join("verifier", "reward.txt"),
		filepath.Join("verifier", "test-stdout.txt"),
		filepath.Join("verifier", "test-stderr.txt"),
	}
	entry := retryEvidenceEntry{Trial: trial, Retry: retry, ExceptionClass: exceptionClass, CreatedAt: time.Now().UTC()}
	for _, relative := range candidates {
		raw, err := os.ReadFile(filepath.Join(trialDir, relative))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", relative, err)
		}
		redacted := []byte(commandlog.RedactText(string(raw)))
		target := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, redacted, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", relative, err)
		}
		entry.Files = append(entry.Files, retryEvidenceFile{Path: target, SHA256: sha256Evidence(redacted), Bytes: len(redacted)})
	}
	if len(entry.Files) == 0 {
		return fmt.Errorf("trial evidence files not found")
	}
	m.manifest.Entries = append(m.manifest.Entries, entry)
	m.manifest.UpdatedAt = time.Now().UTC()
	return m.writeRetryEvidenceManifest()
}

func (m *jobMonitor) writeRetryEvidenceManifest() error {
	root := filepath.Join(m.outputDir, "retry_evidence")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m.manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "manifest.json"), append(raw, '\n'), 0o600)
}

func (m *jobMonitor) retryEvidenceManifestPath() string {
	if len(m.manifest.Entries) == 0 {
		return ""
	}
	return filepath.Join(m.outputDir, "retry_evidence", "manifest.json")
}
