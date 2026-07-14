package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	LocalPatchProviderID  = "local_patch"
	AgentRepairProviderID = "agent_repair"
)

// FindingBundle is the structured, revision-bound repair input consumed by
// both manual and Agent providers. Free-form guidance supplements a finding;
// it does not replace machine-readable checker/evidence facts.
type FindingBundle struct {
	Format           string          `json:"format"`
	RevisionID       string          `json:"revision_id"`
	RevisionDigest   string          `json:"revision_digest"`
	Findings         []RepairFinding `json:"findings"`
	OperatorGuidance string          `json:"operator_guidance,omitempty"`
}

type RepairFinding struct {
	CheckerID           string   `json:"checker_id"`
	StageKey            string   `json:"stage_key"`
	CheckID             string   `json:"check_id"`
	Severity            string   `json:"severity"`
	Message             string   `json:"message"`
	StderrSummary       string   `json:"stderr_summary,omitempty"`
	ReportArtifactID    string   `json:"report_artifact_id,omitempty"`
	ReportContentDigest string   `json:"report_content_digest,omitempty"`
	AttemptedFixes      []string `json:"attempted_fixes,omitempty"`
}

func (bundle FindingBundle) Validate(revisionID, digest string) error {
	if strings.TrimSpace(bundle.Format) != "harbor.findings.v1" {
		return fmt.Errorf("findings format must be harbor.findings.v1")
	}
	if bundle.RevisionID != revisionID || bundle.RevisionDigest != digest {
		return fmt.Errorf("findings do not bind the candidate base revision")
	}
	if len(bundle.Findings) == 0 {
		return fmt.Errorf("findings bundle must contain at least one finding")
	}
	for index, finding := range bundle.Findings {
		if strings.TrimSpace(finding.CheckerID) == "" || strings.TrimSpace(finding.StageKey) == "" ||
			strings.TrimSpace(finding.CheckID) == "" || strings.TrimSpace(finding.Severity) == "" || strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("finding %d is incomplete", index)
		}
		if err := store.ValidateUUIDv7(finding.ReportArtifactID); err != nil {
			return fmt.Errorf("finding %d report artifact ID: %w", index, err)
		}
		if err := workflowkit.Fingerprint(finding.ReportContentDigest).Validate(); err != nil {
			return fmt.Errorf("finding %d report content digest: %w", index, err)
		}
	}
	return nil
}

// TaskChangeRequest is the typed user intent passed into the ChangeProvider
// transaction. Payload must conform to the named provider's schema; raw maps
// are intentionally not exposed to continuation planning.
type TaskChangeRequest struct {
	ProviderID      string          `json:"provider_id"`
	OperationKey    string          `json:"operation_key"`
	Payload         json.RawMessage `json:"payload"`
	Findings        FindingBundle   `json:"findings"`
	MaxRepairRounds int             `json:"max_repair_rounds,omitempty"`

	// Repair-session linkage is application-internal. Only the automatic repair
	// coordinator can continue a session; external callers always create its
	// root command through the normal typed ChangeProvider flow.
	repairSessionID    string
	repairRoundOrdinal int
}

type ChangeProviderRequest struct {
	Candidate    store.RevisionCandidate
	Checkout     string
	Payload      json.RawMessage
	Findings     FindingBundle
	Actor        string
	Reason       string
	RoundOrdinal int
	Timeout      time.Duration
}

type ChangeProviderReceipt struct {
	Format       string   `json:"format"`
	ProviderID   string   `json:"provider_id"`
	ChangedPaths []string `json:"changed_paths,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	AgentModel   string   `json:"agent_model,omitempty"`
	AgentOutput  string   `json:"agent_output,omitempty"`
	Reconciled   bool     `json:"reconciled,omitempty"`
}

// ChangeProvider never receives a sealed snapshot path. It may write only to
// the checkout supplied by RevisionCandidateService, then its result is
// independently validated and digested by the application layer.
type ChangeProvider interface {
	ID() string
	ValidatePayload(json.RawMessage) (json.RawMessage, error)
	Apply(context.Context, ChangeProviderRequest) (ChangeProviderReceipt, error)
}

// LocalPatchProvider applies a constrained unified diff in process. It has no
// executable, PATH, shell, or provider dependency: the candidate checkout and
// the strict Harbor file policy are its complete authority boundary.
type LocalPatchProvider struct{}

func (LocalPatchProvider) ID() string { return LocalPatchProviderID }

type localPatchPayload struct {
	Format string `json:"format"`
	Diff   string `json:"diff"`
}

func (provider LocalPatchProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	var payload localPatchPayload
	if err := decodeProviderPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Format != "harbor.local-unified-diff.v1" || strings.TrimSpace(payload.Diff) == "" {
		return nil, fmt.Errorf("local patch payload requires harbor.local-unified-diff.v1 and diff")
	}
	rewritten, _, err := normalizeUnifiedDiff(payload.Diff)
	if err != nil {
		return nil, err
	}
	payload.Diff = rewritten
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func (provider LocalPatchProvider) Apply(ctx context.Context, request ChangeProviderRequest) (ChangeProviderReceipt, error) {
	var payload localPatchPayload
	if err := decodeProviderPayload(request.Payload, &payload); err != nil {
		return ChangeProviderReceipt{}, err
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(request.Checkout); err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("validate candidate before patch: %w", err)
	}
	diff, paths, err := normalizeUnifiedDiff(payload.Diff)
	if err != nil {
		return ChangeProviderReceipt{}, err
	}
	if err := applyCanonicalUnifiedDiff(ctx, request.Checkout, diff); err != nil {
		return ChangeProviderReceipt{}, err
	}
	return ChangeProviderReceipt{
		Format: "harbor.local-patch-receipt.v1", ProviderID: provider.ID(), ChangedPaths: paths,
		Summary: "applied canonical unified diff " + digestText(diff),
	}, nil
}

type canonicalUnifiedDiff struct {
	files []canonicalUnifiedFilePatch
}

type canonicalUnifiedFilePatch struct {
	path  string
	hunks []canonicalUnifiedHunk
}

type canonicalUnifiedHunk struct {
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []canonicalUnifiedHunkLine
}

type canonicalUnifiedHunkLine struct {
	kind      byte
	text      string
	noNewline bool
}

// applyCanonicalUnifiedDiff applies only the normalized format emitted by
// normalizeUnifiedDiff. It reads and applies every target in memory before
// staging any replacement. A stale hunk in one file therefore cannot write an
// earlier file. Once all replacements are staged, each target rename is atomic
// and a failed later rename restores already replaced targets from staged
// originals as far as the local filesystem permits.
func applyCanonicalUnifiedDiff(ctx context.Context, checkout, diff string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	parsed, err := parseCanonicalUnifiedDiff(diff)
	if err != nil {
		return err
	}
	targets := make([]stagedCandidatePatchTarget, 0, len(parsed.files))
	seenPaths := make(map[string]struct{}, len(parsed.files))
	for _, file := range parsed.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := seenPaths[file.path]; exists {
			return fmt.Errorf("unified diff contains multiple file sections for %s", file.path)
		}
		seenPaths[file.path] = struct{}{}
		path := filepath.Join(checkout, filepath.FromSlash(file.path))
		contents, mode, readErr := readRegularCandidatePatchTarget(path)
		if readErr != nil {
			return fmt.Errorf("read candidate patch target %s: %w", file.path, readErr)
		}
		updated, applyErr := applyCanonicalUnifiedFilePatch(contents, file)
		if applyErr != nil {
			return fmt.Errorf("apply unified diff to %s: %w", file.path, applyErr)
		}
		targets = append(targets, stagedCandidatePatchTarget{
			name: file.path, path: path, original: contents, replacement: updated, mode: mode,
		})
	}
	for index := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := targets[index].stage(); err != nil {
			cleanupStagedCandidatePatchTargets(targets)
			return fmt.Errorf("stage candidate patch target %s: %w", targets[index].name, err)
		}
	}
	defer cleanupStagedCandidatePatchTargets(targets)

	// Recheck every original before the first rename. This closes the normal
	// stale-checkout window without treating a caller-supplied path as trusted.
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		contents, mode, readErr := readRegularCandidatePatchTarget(target.path)
		if readErr != nil {
			return fmt.Errorf("recheck candidate patch target %s: %w", target.name, readErr)
		}
		if mode != target.mode || !bytes.Equal(contents, target.original) {
			return fmt.Errorf("candidate patch target %s changed while unified diff was prepared", target.name)
		}
	}
	for index := range targets {
		if err := os.Rename(targets[index].replacementPath, targets[index].path); err != nil {
			restoreErr := restoreStagedCandidatePatchTargets(targets[:index])
			if restoreErr != nil {
				return fmt.Errorf("commit candidate patch target %s: %w; rollback earlier targets: %v", targets[index].name, err, restoreErr)
			}
			return fmt.Errorf("commit candidate patch target %s: %w", targets[index].name, err)
		}
		targets[index].replacementPath = ""
	}
	return nil
}

type stagedCandidatePatchTarget struct {
	name            string
	path            string
	original        []byte
	replacement     []byte
	mode            os.FileMode
	replacementPath string
	rollbackPath    string
}

func (target *stagedCandidatePatchTarget) stage() error {
	replacementPath, err := stageCandidatePatchFile(target.path, target.replacement, target.mode)
	if err != nil {
		return err
	}
	target.replacementPath = replacementPath
	rollbackPath, err := stageCandidatePatchFile(target.path, target.original, target.mode)
	if err != nil {
		_ = os.Remove(target.replacementPath)
		target.replacementPath = ""
		return err
	}
	target.rollbackPath = rollbackPath
	return nil
}

func stageCandidatePatchFile(targetPath string, contents []byte, mode os.FileMode) (string, error) {
	file, err := os.CreateTemp(filepath.Dir(targetPath), ".harbor-patch-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	cleanup := func(cause error) (string, error) {
		_ = file.Close()
		_ = os.Remove(path)
		return "", cause
	}
	if err := file.Chmod(mode.Perm()); err != nil {
		return cleanup(err)
	}
	if _, err := file.Write(contents); err != nil {
		return cleanup(err)
	}
	if err := file.Sync(); err != nil {
		return cleanup(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func cleanupStagedCandidatePatchTargets(targets []stagedCandidatePatchTarget) {
	for _, target := range targets {
		if target.replacementPath != "" {
			_ = os.Remove(target.replacementPath)
		}
		if target.rollbackPath != "" {
			_ = os.Remove(target.rollbackPath)
		}
	}
}

func restoreStagedCandidatePatchTargets(targets []stagedCandidatePatchTarget) error {
	var restoreErr error
	for index := len(targets) - 1; index >= 0; index-- {
		target := &targets[index]
		if target.rollbackPath == "" {
			continue
		}
		if err := os.Rename(target.rollbackPath, target.path); err != nil && restoreErr == nil {
			restoreErr = fmt.Errorf("restore %s: %w", target.name, err)
		}
		target.rollbackPath = ""
	}
	return restoreErr
}

func parseCanonicalUnifiedDiff(diff string) (canonicalUnifiedDiff, error) {
	if !utf8.ValidString(diff) {
		return canonicalUnifiedDiff{}, fmt.Errorf("local unified diff must be valid UTF-8 text")
	}
	lines := strings.Split(strings.TrimSuffix(diff, "\n"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return canonicalUnifiedDiff{}, fmt.Errorf("local unified diff is empty")
	}
	parsed := canonicalUnifiedDiff{}
	for index := 0; index < len(lines); {
		if !strings.HasPrefix(lines[index], "--- ") {
			return canonicalUnifiedDiff{}, fmt.Errorf("expected --- file header at diff line %d", index+1)
		}
		oldPath := strings.TrimPrefix(lines[index], "--- ")
		index++
		if index >= len(lines) || !strings.HasPrefix(lines[index], "+++ ") {
			return canonicalUnifiedDiff{}, fmt.Errorf("expected +++ file header after diff line %d", index)
		}
		newPath := strings.TrimPrefix(lines[index], "+++ ")
		if oldPath != newPath {
			return canonicalUnifiedDiff{}, fmt.Errorf("normalized unified diff attempts to rename %s to %s", oldPath, newPath)
		}
		file := canonicalUnifiedFilePatch{path: oldPath}
		index++
		for index < len(lines) && !strings.HasPrefix(lines[index], "--- ") {
			if !strings.HasPrefix(lines[index], "@@ ") {
				return canonicalUnifiedDiff{}, fmt.Errorf("expected hunk header at diff line %d", index+1)
			}
			hunk, next, err := parseCanonicalUnifiedHunk(lines, index)
			if err != nil {
				return canonicalUnifiedDiff{}, err
			}
			file.hunks = append(file.hunks, hunk)
			index = next
		}
		if len(file.hunks) == 0 {
			return canonicalUnifiedDiff{}, fmt.Errorf("unified diff for %s has no hunks", file.path)
		}
		parsed.files = append(parsed.files, file)
	}
	return parsed, nil
}

func parseCanonicalUnifiedHunk(lines []string, start int) (canonicalUnifiedHunk, int, error) {
	header := lines[start]
	if !strings.HasPrefix(header, "@@ ") {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid hunk header at diff line %d", start+1)
	}
	ranges, _, found := strings.Cut(strings.TrimPrefix(header, "@@ "), " @@")
	if !found {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid hunk header at diff line %d", start+1)
	}
	parts := strings.Fields(ranges)
	if len(parts) != 2 {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid hunk range at diff line %d", start+1)
	}
	oldStart, oldCount, err := parseCanonicalUnifiedRange(parts[0], '-')
	if err != nil {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid old hunk range at diff line %d: %w", start+1, err)
	}
	newStart, newCount, err := parseCanonicalUnifiedRange(parts[1], '+')
	if err != nil {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid new hunk range at diff line %d: %w", start+1, err)
	}
	hunk := canonicalUnifiedHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	oldSeen, newSeen := 0, 0
	index := start + 1
	for index < len(lines) && !strings.HasPrefix(lines[index], "@@ ") && !strings.HasPrefix(lines[index], "--- ") {
		line := lines[index]
		if line == "\\ No newline at end of file" {
			if len(hunk.lines) == 0 {
				return canonicalUnifiedHunk{}, start, fmt.Errorf("newline marker without a hunk line at diff line %d", index+1)
			}
			hunk.lines[len(hunk.lines)-1].noNewline = true
			index++
			continue
		}
		if len(line) == 0 || (line[0] != ' ' && line[0] != '-' && line[0] != '+') {
			return canonicalUnifiedHunk{}, start, fmt.Errorf("invalid hunk line at diff line %d", index+1)
		}
		entry := canonicalUnifiedHunkLine{kind: line[0], text: line[1:]}
		switch entry.kind {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
		case '+':
			newSeen++
		}
		if oldSeen > oldCount || newSeen > newCount {
			return canonicalUnifiedHunk{}, start, fmt.Errorf("hunk line counts exceed header at diff line %d", index+1)
		}
		hunk.lines = append(hunk.lines, entry)
		index++
	}
	if oldSeen != oldCount || newSeen != newCount {
		return canonicalUnifiedHunk{}, start, fmt.Errorf("hunk line counts do not match header")
	}
	return hunk, index, nil
}

func parseCanonicalUnifiedRange(value string, prefix byte) (int, int, error) {
	if len(value) < 2 || value[0] != prefix {
		return 0, 0, fmt.Errorf("missing %c range prefix", prefix)
	}
	parts := strings.Split(strings.TrimPrefix(value, string(prefix)), ",")
	if len(parts) > 2 || parts[0] == "" {
		return 0, 0, fmt.Errorf("malformed range %q", value)
	}
	start, err := strconv.Atoi(parts[0])
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("invalid range start %q", parts[0])
	}
	count := 1
	if len(parts) == 2 {
		if parts[1] == "" {
			return 0, 0, fmt.Errorf("missing range count")
		}
		count, err = strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("invalid range count %q", parts[1])
		}
	}
	if start == 0 && count != 0 {
		return 0, 0, fmt.Errorf("range start zero requires count zero")
	}
	return start, count, nil
}

func applyCanonicalUnifiedFilePatch(contents []byte, patch canonicalUnifiedFilePatch) ([]byte, error) {
	if !utf8.Valid(contents) {
		return nil, fmt.Errorf("candidate target is not valid UTF-8 text")
	}
	original, trailingNewline := splitCanonicalPatchLines(string(contents))
	updated := make([]string, 0, len(original))
	cursor := 0
	finalTrailingNewline := trailingNewline
	for _, hunk := range patch.hunks {
		target := hunk.oldStart
		if target > 0 {
			target--
		}
		if target < cursor || target > len(original) {
			return nil, fmt.Errorf("hunk starts outside candidate file")
		}
		updated = append(updated, original[cursor:target]...)
		cursor = target
		oldSeen, newSeen := 0, 0
		for _, line := range hunk.lines {
			switch line.kind {
			case ' ':
				if cursor >= len(original) || original[cursor] != line.text {
					return nil, fmt.Errorf("context does not match candidate file")
				}
				updated = append(updated, original[cursor])
				cursor++
				oldSeen++
				newSeen++
			case '-':
				if cursor >= len(original) || original[cursor] != line.text {
					return nil, fmt.Errorf("removed line does not match candidate file")
				}
				cursor++
				oldSeen++
			case '+':
				updated = append(updated, line.text)
				newSeen++
			}
			if line.noNewline {
				switch line.kind {
				case '-', ' ':
					trailingNewline = false
				case '+':
					finalTrailingNewline = false
				}
			}
		}
		if oldSeen != hunk.oldCount || newSeen != hunk.newCount {
			return nil, fmt.Errorf("hunk counts do not match parsed lines")
		}
	}
	updated = append(updated, original[cursor:]...)
	if len(updated) == 0 {
		return []byte{}, nil
	}
	if finalTrailingNewline && trailingNewline {
		return []byte(strings.Join(updated, "\n") + "\n"), nil
	}
	return []byte(strings.Join(updated, "\n")), nil
}

func splitCanonicalPatchLines(value string) ([]string, bool) {
	if value == "" {
		return nil, false
	}
	trailingNewline := strings.HasSuffix(value, "\n")
	if trailingNewline {
		value = strings.TrimSuffix(value, "\n")
	}
	return strings.Split(value, "\n"), trailingNewline
}

func readRegularCandidatePatchTarget(path string) ([]byte, os.FileMode, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("candidate patch target is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	contents, readErr := io.ReadAll(file)
	stat, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil {
		return nil, 0, readErr
	}
	if statErr != nil {
		return nil, 0, statErr
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	if stat.Mode()&os.ModeSymlink != 0 || !stat.Mode().IsRegular() || !os.SameFile(before, stat) {
		return nil, 0, fmt.Errorf("candidate patch target changed while opening")
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(stat, after) {
		return nil, 0, fmt.Errorf("candidate patch target changed while reading")
	}
	return contents, stat.Mode().Perm(), nil
}

type AgentRepairProvider struct {
	Agent agent.Runtime
}

func (AgentRepairProvider) ID() string { return AgentRepairProviderID }

type agentRepairPayload struct {
	Format          string `json:"format"`
	Guidance        string `json:"guidance,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

func (provider AgentRepairProvider) ValidatePayload(raw json.RawMessage) (json.RawMessage, error) {
	var payload agentRepairPayload
	if err := decodeProviderPayload(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Format != "harbor.agent-repair.v1" {
		return nil, fmt.Errorf("agent repair payload requires harbor.agent-repair.v1")
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func (provider AgentRepairProvider) Apply(ctx context.Context, request ChangeProviderRequest) (ChangeProviderReceipt, error) {
	if provider.Agent == nil {
		return ChangeProviderReceipt{}, fmt.Errorf("agent repair provider is not configured")
	}
	var payload agentRepairPayload
	if err := decodeProviderPayload(request.Payload, &payload); err != nil {
		return ChangeProviderReceipt{}, err
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(request.Checkout); err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("validate candidate before agent repair: %w", err)
	}
	findings, err := json.Marshal(request.Findings)
	if err != nil {
		return ChangeProviderReceipt{}, err
	}
	timeoutSeconds := int(request.Timeout.Round(time.Second) / time.Second)
	if timeoutSeconds <= 0 {
		return ChangeProviderReceipt{}, fmt.Errorf("agent repair timeout is required")
	}
	prompt := strings.Join([]string{
		"Repair the Harbor task only inside the supplied isolated candidate checkout.",
		"Do not modify paths outside the checkout. Preserve the strict Harbor V2 file policy and make the smallest coherent correction.",
		"Structured findings:", string(findings),
		"Operator guidance:", strings.TrimSpace(payload.Guidance),
	}, "\n")
	result, err := agent.RunTurn(ctx, provider.Agent, agent.TurnRequest{
		ProjectPath: request.Checkout, Prompt: prompt, Model: payload.Model, ReasoningEffort: payload.ReasoningEffort,
		SandboxMode: "workspace-write", SandboxPolicy: "workspace-write", NetworkAccess: false,
		WorkspaceRoots: []string{request.Checkout}, TimeoutSeconds: timeoutSeconds, MaxOutputBytes: 2 << 20,
		LogPath: filepath.Join(filepath.Dir(request.Checkout), "agent-repair.log"),
	})
	if err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("run agent repair: %w", err)
	}
	return ChangeProviderReceipt{
		Format: "harbor.agent-repair-receipt.v1", ProviderID: provider.ID(), AgentModel: strings.TrimSpace(result.Model),
		AgentOutput: truncateProviderText(result.Text, 12000), Summary: "agent repair completed",
	}, nil
}

func decodeProviderPayload(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		return fmt.Errorf("provider payload is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode provider payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("provider payload contains multiple JSON values")
		}
		return fmt.Errorf("decode trailing provider payload: %w", err)
	}
	return nil
}

func digestText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateProviderText(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximum {
		return value
	}
	return value[:maximum] + "..."
}

// normalizeUnifiedDiff validates and canonicalizes a standard text unified
// diff. Only files in the strict managed task policy can be named. Header
// paths are rewritten to canonical relative paths before invoking patch, so
// a/ and b/ prefixes cannot escape the candidate checkout.
func normalizeUnifiedDiff(raw string) (string, []string, error) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasSuffix(raw, "\n") {
		raw += "\n"
	}
	allowed := make(map[string]struct{})
	for _, file := range taskpolicy.CanonicalFiles() {
		allowed[file.Path] = struct{}{}
	}
	lines := strings.Split(raw, "\n")
	var output []string
	var paths []string
	pendingOld := ""
	for _, line := range lines[:len(lines)-1] {
		switch {
		case strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index "):
			// Header metadata is unnecessary to patch and may contain a path
			// spelling different from the authoritative ---/+++ file names.
			continue
		case strings.HasPrefix(line, "new file mode") || strings.HasPrefix(line, "deleted file mode") || strings.HasPrefix(line, "rename ") || strings.HasPrefix(line, "similarity index") || strings.HasPrefix(line, "Binary files "):
			return "", nil, fmt.Errorf("local unified diff may not add, delete, rename, or contain binary managed files")
		case strings.HasPrefix(line, "--- "):
			path, err := canonicalPatchPath(strings.TrimPrefix(line, "--- "), allowed)
			if err != nil {
				return "", nil, err
			}
			pendingOld = path
			output = append(output, "--- "+path)
		case strings.HasPrefix(line, "+++ "):
			if pendingOld == "" {
				return "", nil, fmt.Errorf("unified diff has +++ header without --- header")
			}
			path, err := canonicalPatchPath(strings.TrimPrefix(line, "+++ "), allowed)
			if err != nil {
				return "", nil, err
			}
			if path != pendingOld {
				return "", nil, fmt.Errorf("unified diff may not rename %s to %s", pendingOld, path)
			}
			paths = append(paths, path)
			pendingOld = ""
			output = append(output, "+++ "+path)
		default:
			output = append(output, line)
		}
	}
	if pendingOld != "" || len(paths) == 0 {
		return "", nil, fmt.Errorf("unified diff must contain paired --- and +++ headers")
	}
	sort.Strings(paths)
	unique := paths[:0]
	for _, path := range paths {
		if len(unique) == 0 || unique[len(unique)-1] != path {
			unique = append(unique, path)
		}
	}
	return strings.Join(output, "\n") + "\n", unique, nil
}

func canonicalPatchPath(header string, allowed map[string]struct{}) (string, error) {
	header = strings.TrimSpace(header)
	if before, _, found := strings.Cut(header, "\t"); found {
		header = before
	}
	header = strings.TrimSpace(header)
	header = strings.TrimPrefix(header, "a/")
	header = strings.TrimPrefix(header, "b/")
	if header == "/dev/null" || filepath.IsAbs(header) {
		return "", fmt.Errorf("unified diff path %q is not an existing managed task file", header)
	}
	path := filepath.ToSlash(filepath.Clean(header))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "\\") {
		return "", fmt.Errorf("unified diff path %q escapes candidate checkout", header)
	}
	if _, ok := allowed[path]; !ok {
		return "", fmt.Errorf("unified diff path %q is outside strict Harbor file policy", path)
	}
	return path, nil
}
