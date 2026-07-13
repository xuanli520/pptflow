package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/workflow"
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

type LocalPatchProvider struct {
	// PatchPath is injectable for focused tests. Production uses PATH lookup
	// for the standard POSIX patch utility and never invokes a shell.
	PatchPath string
}

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
	if err := harborrun.ValidateManagedTaskSnapshotV2(request.Checkout); err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("validate candidate before patch: %w", err)
	}
	diff, paths, err := normalizeUnifiedDiff(payload.Diff)
	if err != nil {
		return ChangeProviderReceipt{}, err
	}
	diffFile, err := os.CreateTemp("", "harbor-candidate-*.diff")
	if err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("create temporary diff: %w", err)
	}
	diffPath := diffFile.Name()
	defer os.Remove(diffPath)
	if _, err := diffFile.WriteString(diff); err != nil {
		_ = diffFile.Close()
		return ChangeProviderReceipt{}, err
	}
	if err := diffFile.Chmod(0o600); err != nil {
		_ = diffFile.Close()
		return ChangeProviderReceipt{}, err
	}
	if err := diffFile.Close(); err != nil {
		return ChangeProviderReceipt{}, err
	}
	patchPath := strings.TrimSpace(provider.PatchPath)
	if patchPath == "" {
		patchPath, err = exec.LookPath("patch")
		if err != nil {
			return ChangeProviderReceipt{}, fmt.Errorf("local patch provider requires patch executable: %w", err)
		}
	}
	command := exec.CommandContext(ctx, patchPath, "--batch", "--forward", "--posix", "--fuzz=0", "-p0", "-i", diffPath)
	command.Dir = request.Checkout
	output, err := command.CombinedOutput()
	if err != nil {
		return ChangeProviderReceipt{}, fmt.Errorf("apply unified diff: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return ChangeProviderReceipt{
		Format: "harbor.local-patch-receipt.v1", ProviderID: provider.ID(), ChangedPaths: paths,
		Summary: "applied canonical unified diff " + digestText(diff),
	}, nil
}

type AgentRepairProvider struct {
	Agent workflow.AgentRuntime
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
	if err := harborrun.ValidateManagedTaskSnapshotV2(request.Checkout); err != nil {
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
	result, err := workflow.RunAgentTurn(ctx, provider.Agent, workflow.AgentTurnRequest{
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
