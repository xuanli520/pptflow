package authoringharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	ReportFormat  = "harbor.standard-authoring.docker-harness.v1"
	ReportVersion = "1"

	ModeDockerfileBuild Mode = "dockerfile_build"
	ModeInitialOracle   Mode = "initial_oracle"
)

// Mode is selected by the frozen authoring stage, never by model-provided
// tool arguments.
type Mode string

func (mode Mode) Validate() error {
	switch mode {
	case ModeDockerfileBuild, ModeInitialOracle:
		return nil
	default:
		return fmt.Errorf("unsupported Standard authoring harness mode %q", mode)
	}
}

// Request contains only frozen execution identities. The validator derives
// the task root from the managed attempt-workspace layout; callers cannot
// provide a filesystem path or command.
type Request struct {
	Mode           Mode
	RunID          string
	StageKey       workflowkit.StageKey
	StageAttemptID string
}

// StepResult is bounded feedback from one host-owned Docker operation.
type StepResult struct {
	Step              string                  `json:"step"`
	Passed            bool                    `json:"passed"`
	ExitCode          int                     `json:"exit_code"`
	Findings          []string                `json:"findings"`
	StdoutTail        string                  `json:"stdout_tail"`
	StderrTail        string                  `json:"stderr_tail"`
	OutputFingerprint workflowkit.Fingerprint `json:"output_fingerprint"`
}

// Result is both dynamic-tool feedback and the source of the immutable stage
// report artifact. ReportJSON is the canonical encoding of every other field.
type Result struct {
	Format            string                  `json:"format"`
	Version           string                  `json:"version"`
	Mode              Mode                    `json:"mode"`
	RunID             string                  `json:"run_id"`
	StageKey          workflowkit.StageKey    `json:"stage_key"`
	StageAttemptID    string                  `json:"stage_attempt_id"`
	Passed            bool                    `json:"passed"`
	Step              string                  `json:"step"`
	ExitCode          int                     `json:"exit_code"`
	Findings          []string                `json:"findings"`
	StdoutTail        string                  `json:"stdout_tail"`
	StderrTail        string                  `json:"stderr_tail"`
	CandidateDigest   workflowkit.Fingerprint `json:"candidate_digest"`
	EnvironmentDigest workflowkit.Fingerprint `json:"environment_digest"`
	ImageID           string                  `json:"image_id,omitempty"`
	ImageReused       bool                    `json:"image_reused"`
	Steps             []StepResult            `json:"steps"`
	ReportJSON        []byte                  `json:"-"`
}

// Validator is the narrow host-owned capability consumed by authoring agent
// executors. Implementations must not accept commands or arbitrary paths.
type Validator interface {
	Validate(context.Context, Request) (Result, error)
}

// Finalize validates a result and attaches its deterministic JSON report.
func Finalize(result Result) (Result, error) {
	result.Format = ReportFormat
	result.Version = ReportVersion
	if result.Findings == nil {
		result.Findings = []string{}
	}
	if result.Steps == nil {
		result.Steps = []StepResult{}
	}
	for index := range result.Steps {
		if result.Steps[index].Findings == nil {
			result.Steps[index].Findings = []string{}
		}
	}
	if err := result.validate(false); err != nil {
		return Result{}, err
	}
	report, err := canonicalReportJSON(result)
	if err != nil {
		return Result{}, fmt.Errorf("marshal Standard authoring harness report: %w", err)
	}
	result.ReportJSON = report
	return result, nil
}

// ValidateReportJSON proves that ReportJSON is canonical and bound to the
// structured result returned alongside it.
func (result Result) ValidateReportJSON() error {
	if err := result.validate(true); err != nil {
		return err
	}
	want := result
	want.ReportJSON = nil
	encoded, err := canonicalReportJSON(want)
	if err != nil {
		return fmt.Errorf("marshal Standard authoring harness report: %w", err)
	}
	if !bytes.Equal(result.ReportJSON, encoded) {
		return errors.New("Standard authoring harness report JSON is not canonical")
	}
	return nil
}

func canonicalReportJSON(result Result) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return nil, errors.New("Standard authoring harness report encoder omitted terminal newline")
	}
	return append([]byte(nil), encoded[:len(encoded)-1]...), nil
}

func (result Result) validate(requireReport bool) error {
	if result.Format != ReportFormat || result.Version != ReportVersion {
		return errors.New("Standard authoring harness report identity is invalid")
	}
	if err := result.Mode.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(result.RunID) == "" || strings.TrimSpace(string(result.StageKey)) == "" || strings.TrimSpace(result.StageAttemptID) == "" {
		return errors.New("Standard authoring harness execution identity is incomplete")
	}
	if strings.TrimSpace(result.Step) == "" {
		return errors.New("Standard authoring harness report step is required")
	}
	if err := result.CandidateDigest.Validate(); err != nil {
		return fmt.Errorf("Standard authoring harness candidate digest: %w", err)
	}
	if err := result.EnvironmentDigest.Validate(); err != nil {
		return fmt.Errorf("Standard authoring harness environment digest: %w", err)
	}
	if result.Passed && len(result.Findings) != 0 {
		return errors.New("passing Standard authoring harness report cannot contain findings")
	}
	if len(result.Steps) == 0 {
		return errors.New("Standard authoring harness report requires at least one step")
	}
	last := result.Steps[len(result.Steps)-1]
	if last.Step != result.Step || last.Passed != result.Passed || last.ExitCode != result.ExitCode || !slices.Equal(last.Findings, result.Findings) || last.StdoutTail != result.StdoutTail || last.StderrTail != result.StderrTail {
		return errors.New("Standard authoring harness summary does not match its final step")
	}
	for _, step := range result.Steps {
		if strings.TrimSpace(step.Step) == "" {
			return errors.New("Standard authoring harness step name is required")
		}
		if err := step.OutputFingerprint.Validate(); err != nil {
			return fmt.Errorf("Standard authoring harness step output fingerprint: %w", err)
		}
		if step.Passed && len(step.Findings) != 0 {
			return errors.New("passing Standard authoring harness step cannot contain findings")
		}
	}
	if requireReport && len(result.ReportJSON) == 0 {
		return errors.New("Standard authoring harness canonical report JSON is required")
	}
	return nil
}
