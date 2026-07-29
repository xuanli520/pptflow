package workflowadapter

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	StandardAuthoringValidationRepairContextArtifact      = "validation_repair_context"
	StandardAuthoringValidationRepairContextSchemaVersion = "harbor.standard-authoring.validation-repair-context.v1"
	StandardAuthoringValidationRepairContextFormat        = StandardAuthoringValidationRepairContextSchemaVersion
	StandardAuthoringValidationRepairContextVersion       = "1"
)

// StandardAuthoringValidationRepairContext is the minimal host-derived repair
// input for authoring_repair. It intentionally contains only immutable
// identities and bounded diagnostics already present in validation_receipt.
type StandardAuthoringValidationRepairContext struct {
	Format            string                        `json:"format"`
	Version           string                        `json:"version"`
	CandidateDigest   workflowkit.Fingerprint       `json:"candidate_digest"`
	ReceiptDigest     workflowkit.Fingerprint       `json:"receipt_digest"`
	ValidationVerdict workflowkit.ValidationVerdict `json:"validation_verdict"`
	FailureCode       workflowkit.AgentFailureCode  `json:"failure_code,omitempty"`
	FailedStep        string                        `json:"failed_step,omitempty"`
	ExitCode          int                           `json:"exit_code"`
	TestStarted       bool                          `json:"test_started"`
	StdoutTail        string                        `json:"stdout_tail,omitempty"`
	StderrTail        string                        `json:"stderr_tail,omitempty"`
	EditableFiles     []string                      `json:"editable_files"`
}

func NewStandardAuthoringValidationRepairContext(receipt workflowkit.ValidationReceipt, editableFiles []string) (StandardAuthoringValidationRepairContext, error) {
	if err := receipt.Validate(); err != nil {
		return StandardAuthoringValidationRepairContext{}, err
	}
	context := StandardAuthoringValidationRepairContext{
		Format: StandardAuthoringValidationRepairContextFormat, Version: StandardAuthoringValidationRepairContextVersion,
		CandidateDigest: receipt.SnapshotDigest, ReceiptDigest: receipt.Digest,
		ValidationVerdict: receipt.Verdict, FailureCode: receipt.FailureCode,
		EditableFiles: append([]string(nil), editableFiles...),
	}
	if len(context.EditableFiles) == 0 {
		context.EditableFiles = []string{
			"instruction.md",
			"task.toml",
			"environment/Dockerfile",
			"solution/solve.sh",
			"tests/test.sh",
			"tests_analysis.json",
		}
	}
	if len(receipt.Diagnostics) != 0 {
		failed := receipt.Diagnostics[len(receipt.Diagnostics)-1]
		for _, diagnostic := range receipt.Diagnostics {
			if diagnostic.ExitCode != 0 {
				failed = diagnostic
				break
			}
		}
		context.FailedStep = failed.CommandID
		context.ExitCode = failed.ExitCode
		context.TestStarted = failed.TestStarted
		context.StdoutTail = failed.StdoutTail
		context.StderrTail = failed.StderrTail
	}
	context = context.Canonical()
	if err := context.Validate(); err != nil {
		return StandardAuthoringValidationRepairContext{}, err
	}
	return context, nil
}

func ParseStandardAuthoringValidationRepairContextJSON(raw []byte) (StandardAuthoringValidationRepairContext, error) {
	if len(raw) == 0 || len(raw) > 64<<10 || !json.Valid(raw) {
		return StandardAuthoringValidationRepairContext{}, fmt.Errorf("Standard authoring validation repair context is invalid")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return StandardAuthoringValidationRepairContext{}, fmt.Errorf("Standard authoring validation repair context: %w", err)
	}
	var context StandardAuthoringValidationRepairContext
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&context); err != nil {
		return StandardAuthoringValidationRepairContext{}, fmt.Errorf("Standard authoring validation repair context: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return StandardAuthoringValidationRepairContext{}, fmt.Errorf("Standard authoring validation repair context has trailing data")
	}
	if err := context.Validate(); err != nil {
		return StandardAuthoringValidationRepairContext{}, err
	}
	return context.Canonical(), nil
}

func (context StandardAuthoringValidationRepairContext) Canonical() StandardAuthoringValidationRepairContext {
	context.EditableFiles = append([]string(nil), context.EditableFiles...)
	sort.Strings(context.EditableFiles)
	return context
}

func (context StandardAuthoringValidationRepairContext) Validate() error {
	if context.Format != StandardAuthoringValidationRepairContextFormat || context.Version != StandardAuthoringValidationRepairContextVersion {
		return fmt.Errorf("Standard authoring validation repair context identity is invalid")
	}
	if err := context.CandidateDigest.Validate(); err != nil {
		return fmt.Errorf("Standard authoring validation repair context candidate digest: %w", err)
	}
	if err := context.ReceiptDigest.Validate(); err != nil {
		return fmt.Errorf("Standard authoring validation repair context receipt digest: %w", err)
	}
	switch context.ValidationVerdict {
	case workflowkit.ValidationPass:
		if context.FailureCode != workflowkit.AgentFailureNone {
			return fmt.Errorf("passing validation repair context cannot carry a failure code")
		}
	case workflowkit.ValidationReject:
		if context.FailureCode == workflowkit.AgentFailureNone {
			return fmt.Errorf("rejected validation repair context requires a failure code")
		}
	default:
		return fmt.Errorf("Standard authoring validation repair context verdict is invalid")
	}
	if strings.TrimSpace(context.FailedStep) != "" && !standardAuthoringValidationRepairIdentifier(context.FailedStep) {
		return fmt.Errorf("Standard authoring validation repair context failed step is invalid")
	}
	for _, tail := range []string{context.StdoutTail, context.StderrTail} {
		if len(tail) > workflowkit.MaxRedactedDiagnosticTail || strings.IndexByte(tail, '\x00') >= 0 {
			return fmt.Errorf("Standard authoring validation repair context diagnostic tail is invalid")
		}
	}
	if len(context.EditableFiles) == 0 {
		return fmt.Errorf("Standard authoring validation repair context editable files are required")
	}
	previous := ""
	for _, file := range context.EditableFiles {
		if err := workflowkit.ValidateCandidateFilePath(file); err != nil {
			return fmt.Errorf("Standard authoring validation repair context editable file: %w", err)
		}
		if previous != "" && file <= previous {
			return fmt.Errorf("Standard authoring validation repair context editable files are not canonical")
		}
		previous = file
	}
	return nil
}

func standardAuthoringValidationRepairIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}
