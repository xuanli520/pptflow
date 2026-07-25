package stageprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/purplevoid/harbor-factory/internal/agent"
	"github.com/purplevoid/harbor-factory/internal/harbor/authoringharness"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringCodexSubmitToolName                 = "harbor_submit_stage_output"
	standardAuthoringCodexOutputSubmissionQuotaDimension = "output_submission"
	standardAuthoringCodexSubmissionFailureQuota         = "standard_authoring_codex_agent_turn.output_submission_quota"
	standardAuthoringCodexSubmissionFailureLease         = "standard_authoring_codex_agent_turn.output_submission_lease_lost"
	standardAuthoringCodexSubmissionFailureAccounting    = "standard_authoring_codex_agent_turn.output_submission_accounting"
	standardAuthoringCodexSubmissionFailureAbsent        = "standard_authoring_codex_agent_turn.output_submission_missing"

	// This host-only representation is the stable input to the receipt digest.
	// It records artifact identity only after the frozen StageDescriptor has
	// supplied it, rather than asking a model to repeat identity fields.
	standardAuthoringCodexCanonicalSubmissionFormat  = "harbor.standard-authoring-codex-stage-submission.v1"
	standardAuthoringCodexCanonicalSubmissionVersion = "1"

	// This is a separate, deployment-pinned schema for v2 solve/test
	// fixed-file receipts. It intentionally has no artifacts/content_base64
	// field: the host reads the exact fixed file after verdict=pass.
	standardAuthoringCodexFixedFileOutputSchemaCanonicalJSON = `{"$id":"harbor.standard-authoring-codex-fixed-file-submit.v1","$schema":"http://json-schema.org/draft-07/schema#","additionalProperties":false,"properties":{"verdict":{"enum":["pass"],"type":"string"}},"required":["verdict"],"type":"object"}`
)

// standardAuthoringCodexOutputSubmission owns the one in-memory authority for
// a candidate accepted during an ephemeral App Server conversation. Invalid
// candidates never leave this object: only their digest and stable diagnostic
// are returned to Codex. The caller publishes its accepted StageExecutionResult
// only after the App Server turn is over.
type standardAuthoringCodexOutputSubmission struct {
	mu sync.Mutex

	request     workflowkit.StageExecutionRequest
	stage       workflowkit.StageDescriptor
	maxBytes    int
	maxAttempts int
	now         func() time.Time

	// environmentPolicy is derived only from the canonical root contract for
	// dockerfile_generate. Keeping that host-owned image constraint on the
	// submission authority prevents a model response from selecting its own
	// base image after it has seen the root contract in the first-turn request.
	environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy
	contractDigest    workflowkit.Fingerprint

	// structuredClaimContract and frozenSourceRoot are populated only by the
	// executor after it has parsed the canonical root contract and re-attested
	// the immutable source workspace. They apply solely to the two structured
	// planning artifacts below; no model-owned workspace is consulted here.
	structuredClaimContract *workflowadapter.AuthoringContract
	frozenSourceRoot        string
	// testsAnalysisRequirementIDs is the exact, host-derived requirement set
	// from the validated proposal and generated plan. It prevents a later
	// analysis from silently dropping or inventing requirement identifiers.
	testsAnalysisRequirementIDs map[string]struct{}

	// fixedFileRelativePath is set only for the v2 workspace-backed solve
	// and test producers. It is selected from the frozen stage key by the
	// host, never supplied by the model. Those stages retain the ordinary
	// output-submission accounting and result ownership, but their tool carries
	// only verdict=pass and their bytes are read from this fixed workspace file.
	fixedFileRelativePath string
	taskRoot              string
	readFixedFile         func(string, string, int64) ([]byte, error)

	currentTurn int
	attempts    int
	accepted    *standardAuthoringCodexAcceptedOutput
	failureCode string
}

type standardAuthoringCodexAcceptedOutput struct {
	result workflowkit.StageExecutionResult
}

type standardAuthoringCodexSubmissionCandidate struct {
	// Pointers preserve the distinction between an omitted/null required value
	// and an explicit empty value. json.Decoder alone otherwise turns both into
	// zero values, which could accidentally make an absent base64 field look
	// like a valid empty artifact.
	Verdict        *workflowkit.Verdict                             `json:"verdict"`
	ContractDigest *string                                          `json:"contract_digest"`
	Artifacts      *[]standardAuthoringCodexSubmissionCandidatePart `json:"artifacts"`
}

// The model deliberately cannot name an artifact, schema, stage, or path.
// Artifact identity comes only from the frozen StageDescriptor held by the
// host, while each array position supplies bytes for that declared output.
type standardAuthoringCodexSubmissionCandidatePart struct {
	ContentBase64 *string `json:"content_base64"`
}

const (
	standardAuthoringCodexTaskProposalFormat      = "harbor.standard-authoring-task-proposal.v2"
	standardAuthoringCodexGeneratedTaskPlanFormat = "harbor.standard-authoring-generated-task-plan.v2"
	standardAuthoringCodexTestsAnalysisFormat     = "harbor.standard-authoring-tests-analysis.v2"
	standardAuthoringCodexStructuredClaimsVersion = "2"
)

var standardAuthoringCodexRequirementIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_-]{0,63}$`)

// standardAuthoringCodexStructuredClaimDocument is intentionally a narrow
// validation shape. It validates the two planning artifacts that make claims
// about the immutable authoring contract and frozen checkout without creating
// a second generic configuration language for other stage outputs.
type standardAuthoringCodexStructuredClaimDocument struct {
	Format         *string                                    `json:"format"`
	Version        *string                                    `json:"version"`
	ContractClaims *standardAuthoringCodexContractClaims      `json:"contract_claims"`
	Requirements   *[]standardAuthoringCodexRequirement       `json:"requirements"`
	SourcePaths    *[]string                                  `json:"source_paths"`
	Packages       *[]standardAuthoringCodexPackage           `json:"packages"`
	Commands       *[]standardAuthoringCodexStructuredCommand `json:"commands"`
}

type standardAuthoringCodexContractClaims struct {
	Title         *string `json:"title"`
	Slug          *string `json:"slug"`
	RepositoryURL *string `json:"repository_url"`
	CommitSHA     *string `json:"commit_sha"`
	BaseImage     *string `json:"base_image"`
	CodeLang      *string `json:"code_lang"`
	TaskType      *string `json:"task_type"`
	Application   *string `json:"application"`
	Is0To1        *bool   `json:"is_0_to_1"`
	SourceRoot    *string `json:"source_root"`
}

type standardAuthoringCodexRequirement struct {
	ID   *string `json:"id"`
	Text *string `json:"text"`
}

type standardAuthoringCodexPackage struct {
	ManifestPath *string `json:"manifest_path"`
}

type standardAuthoringCodexStructuredCommand struct {
	WorkingDirectory *string   `json:"working_directory"`
	Argv             *[]string `json:"argv"`
}

type standardAuthoringCodexCanonicalSubmission struct {
	Format         string                                              `json:"format"`
	Version        string                                              `json:"version"`
	StageKey       workflowkit.StageKey                                `json:"stage_key"`
	StageVersion   string                                              `json:"stage_version"`
	Verdict        workflowkit.Verdict                                 `json:"verdict"`
	ContractDigest string                                              `json:"contract_digest,omitempty"`
	Artifacts      []standardAuthoringCodexCanonicalSubmissionArtifact `json:"artifacts"`
}

type standardAuthoringCodexCanonicalSubmissionArtifact struct {
	Name          string `json:"name"`
	SchemaVersion string `json:"schema_version"`
	ContentBase64 string `json:"content_base64"`
}

type standardAuthoringCodexSubmissionReceipt struct {
	Accepted       bool                    `json:"accepted"`
	Errors         []string                `json:"errors"`
	Remaining      int                     `json:"remaining"`
	Digest         workflowkit.Fingerprint `json:"digest"`
	ContractDigest workflowkit.Fingerprint `json:"contract_digest,omitempty"`
}

func newStandardAuthoringCodexOutputSubmission(request workflowkit.StageExecutionRequest, maxBytes int, maxAttempts int, now func() time.Time, environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy) (*standardAuthoringCodexOutputSubmission, error) {
	if maxBytes <= 0 || maxAttempts <= 0 || request.Charge == nil {
		return nil, errors.New("invalid Standard authoring Codex output submission configuration")
	}
	if err := request.Stage.Validate(); err != nil {
		return nil, fmt.Errorf("validate frozen Standard authoring Codex stage: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	if request.Stage.Key == workflowkit.StageKey(workflowadapter.DockerfileGen) {
		if environmentPolicy == nil || environmentPolicy.Validate() != nil {
			return nil, errors.New("invalid frozen Standard authoring Dockerfile environment policy")
		}
		policyCopy := *environmentPolicy
		environmentPolicy = &policyCopy
	} else {
		environmentPolicy = nil
	}
	contractDigest, err := standardAuthoringCodexBoundContractDigest(request)
	if err != nil {
		return nil, err
	}
	return &standardAuthoringCodexOutputSubmission{
		request: request,
		stage:   request.Stage.Clone(), contractDigest: contractDigest,
		maxBytes: maxBytes, maxAttempts: maxAttempts, now: now, environmentPolicy: environmentPolicy,
	}, nil
}

// setStructuredClaimValidation attaches the canonical root contract and exact
// frozen source root to the two structured planning producers. The caller must
// invoke it only after verifyFrozenSource has succeeded for sourceRoot.
func (submission *standardAuthoringCodexOutputSubmission) setStructuredClaimValidation(contract workflowadapter.AuthoringContract, sourceRoot string) error {
	if submission == nil {
		return errors.New("structured claim submission is unavailable")
	}
	if _, ok := standardAuthoringCodexStructuredClaimFormat(submission.stage); !ok {
		return nil
	}
	canonical, err := contract.Canonical()
	if err != nil || canonical != contract {
		return errors.New("structured claim root contract is invalid")
	}
	root, err := standardAuthoringCodexFrozenSourceRoot(sourceRoot)
	if err != nil {
		return err
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil || submission.attempts != 0 {
		return errors.New("structured claim validation must be configured before submission")
	}
	contractCopy := canonical
	submission.structuredClaimContract = &contractCopy
	submission.frozenSourceRoot = root
	return nil
}

// setTestsAnalysisRequirementValidation binds tests analysis to the same stable
// requirement IDs asserted by both upstream planning artifacts.
func (submission *standardAuthoringCodexOutputSubmission) setTestsAnalysisRequirementValidation(inputs []standardAuthoringCodexInput) error {
	if submission == nil || submission.stage.Key != workflowkit.StageKey(workflowadapter.TestsAnalysis) {
		return nil
	}
	var proposalIDs, planIDs map[string]struct{}
	for _, input := range inputs {
		if input.Name != "task_proposal" && input.Name != "generated_task_files" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(input.ContentBase64)
		if err != nil || workflowkit.SHA256Fingerprint(raw) != input.ContentDigest {
			return errors.New("tests-analysis requirement input is invalid")
		}
		format := standardAuthoringCodexTaskProposalFormat
		if input.Name == "generated_task_files" {
			format = standardAuthoringCodexGeneratedTaskPlanFormat
		}
		ids, err := standardAuthoringCodexStructuredRequirementIDs(raw, format)
		if err != nil {
			return err
		}
		if input.Name == "task_proposal" {
			proposalIDs = ids
		} else {
			planIDs = ids
		}
	}
	if len(proposalIDs) == 0 || len(planIDs) == 0 || !standardAuthoringCodexSameRequirementIDs(proposalIDs, planIDs) {
		return errors.New("tests-analysis requirements do not match the validated planning artifacts")
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil || submission.attempts != 0 {
		return errors.New("tests-analysis validation must be configured before submission")
	}
	submission.testsAnalysisRequirementIDs = proposalIDs
	return nil
}

func standardAuthoringCodexStructuredRequirementIDs(content []byte, expectedFormat string) (map[string]struct{}, error) {
	if len(content) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || rejectDuplicateDeploymentCatalogJSONKeys(content) != nil {
		return nil, errors.New("structured requirement input is invalid")
	}
	var document standardAuthoringCodexStructuredClaimDocument
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("structured requirement input is invalid")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) || document.Format == nil || *document.Format != expectedFormat ||
		document.Version == nil || *document.Version != standardAuthoringCodexStructuredClaimsVersion || document.Requirements == nil || len(*document.Requirements) == 0 {
		return nil, errors.New("structured requirement input is invalid")
	}
	ids := make(map[string]struct{}, len(*document.Requirements))
	for _, requirement := range *document.Requirements {
		if requirement.ID == nil || !standardAuthoringCodexRequirementIDPattern.MatchString(*requirement.ID) || requirement.Text == nil || !standardAuthoringCodexNonEmptyText(*requirement.Text) {
			return nil, errors.New("structured requirement input is invalid")
		}
		if _, duplicate := ids[*requirement.ID]; duplicate {
			return nil, errors.New("structured requirement input is invalid")
		}
		ids[*requirement.ID] = struct{}{}
	}
	return ids, nil
}

func standardAuthoringCodexSameRequirementIDs(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if _, found := right[id]; !found {
			return false
		}
	}
	return true
}

// newStandardAuthoringCodexFixedFileSubmission creates the submission
// authority for the two pre-harness scripts. The resulting stage artifact is
// read from an attempt-scoped fixed file rather than echoed back through the
// model tool call. This keeps arbitrary model output out of the artifact data
// plane while preserving the normal bounded output_submission quota.
func newStandardAuthoringCodexFixedFileSubmission(request workflowkit.StageExecutionRequest, taskRoot string, maxBytes int, maxAttempts int, now func() time.Time) (*standardAuthoringCodexOutputSubmission, error) {
	submission, err := newStandardAuthoringCodexOutputSubmission(request, maxBytes, maxAttempts, now, nil)
	if err != nil {
		return nil, err
	}
	if request.Execution.Workflow.ID != workflowadapter.StandardAuthoringWorkflowTemplateID || request.Execution.Workflow.Version != workflowadapter.StandardAuthoringCurrentTemplateReference().Version {
		return nil, errors.New("Standard authoring Codex fixed-file submission requires the v2 template")
	}
	relative, outputName, _, ok := standardAuthoringCodexFixedFileStageContract(submission.stage)
	expected, found := workflowadapter.StandardAuthoringCurrentWorkflowTemplate().Catalog.Stage(submission.stage.Key)
	if !ok || !found || submission.stage.Version != expected.Version || submission.stage.Plugin.ID != expected.Plugin.ID || submission.stage.Plugin.Version != expected.Plugin.Version ||
		!reflect.DeepEqual(submission.stage.Outputs, expected.Outputs) || !reflect.DeepEqual(submission.stage.Verdicts, expected.Verdicts) ||
		len(submission.stage.Outputs) != 1 || !submission.stage.Outputs[0].Required || submission.stage.Outputs[0].Name != outputName ||
		!reflect.DeepEqual(submission.stage.Verdicts, workflowkit.VerdictPolicy{Allowed: []workflowkit.Verdict{workflowkit.VerdictPass}}) {
		return nil, errors.New("invalid Standard authoring Codex fixed-file stage contract")
	}
	absoluteTaskRoot, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absoluteTaskRoot) != taskRoot {
		return nil, errors.New("invalid Standard authoring Codex fixed-file task workspace")
	}
	submission.fixedFileRelativePath = relative
	// Retain the trusted exact task root only after the constructor has proved
	// it is a clean absolute path. ReadFixedFile re-proves its directory and
	// individual file safety at every submission.
	submission.taskRoot = taskRoot
	submission.readFixedFile = authoringharness.ReadFixedFileWithLimit
	return submission, nil
}

// standardAuthoringCodexPrepareFixedFileWorkspace creates the one parent
// directory selected by the frozen solve/test stage before the model begins.
// The file itself deliberately does not exist yet: a pass without a newly
// authored regular file is rejected by the host reader rather than inheriting
// a deceptively valid placeholder.
func standardAuthoringCodexPrepareFixedFileWorkspace(taskRoot string, stage workflowkit.StageDescriptor) error {
	relative, _, _, ok := standardAuthoringCodexFixedFileStageContract(stage)
	if !ok {
		return errors.New("Standard authoring stage has no fixed-file workspace contract")
	}
	absoluteTaskRoot, err := filepath.Abs(strings.TrimSpace(taskRoot))
	if err != nil || strings.TrimSpace(taskRoot) == "" || filepath.Clean(absoluteTaskRoot) != taskRoot {
		return errors.New("invalid Standard authoring Codex fixed-file task workspace")
	}
	rootInfo, err := os.Lstat(taskRoot)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("Standard authoring Codex fixed-file task root is unsafe")
	}
	directory := filepath.Join(taskRoot, filepath.Dir(filepath.FromSlash(relative)))
	if err := os.Mkdir(directory, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create Standard authoring Codex fixed-file directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() {
		return errors.New("Standard authoring Codex fixed-file directory is unsafe")
	}
	path := filepath.Join(taskRoot, filepath.FromSlash(relative))
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("Standard authoring Codex fixed-file candidate unexpectedly exists")
	}
	return nil
}

func (submission *standardAuthoringCodexOutputSubmission) beginTurn(turn int) error {
	if submission == nil || turn < 1 {
		return errors.New("Standard authoring Codex output submission turn is invalid")
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.failureCode != "" {
		return errors.New("Standard authoring Codex output submission is unavailable")
	}
	submission.currentTurn = turn
	return nil
}

func (submission *standardAuthoringCodexOutputSubmission) dynamicTool() agent.DynamicTool {
	description := standardAuthoringCodexSubmitToolDescription(submission.stage.Key)
	schema := standardAuthoringCodexSubmissionSchemaForContract(submission.stage, submission.contractDigest != "")
	if submission != nil && submission.fixedFileRelativePath != "" {
		description = "Submit the host-selected fixed workspace file only after writing its final raw bytes under task/" + submission.fixedFileRelativePath + ". The only accepted argument is verdict=pass; artifact bytes, names, paths, schema, and validation are host-owned."
		schema = standardAuthoringCodexFixedFileSubmissionSchemaForContract(submission.contractDigest != "")
	}
	return agent.DynamicTool{
		Name:        standardAuthoringCodexSubmitToolName,
		Description: description,
		InputSchema: schema,
		Handler:     submission.handle,
	}
}

func standardAuthoringCodexSubmitToolDescription(stageKey workflowkit.StageKey) string {
	base := "Validate and submit this stage's frozen output candidate. Submit only the allowed verdict and one base64 content value for each declared output, in declared order."
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.InstructionGen), workflowkit.StageKey(workflowadapter.TaskTOMLGen),
		workflowkit.StageKey(workflowadapter.DockerfileGen), workflowkit.StageKey(workflowadapter.SolveGen), workflowkit.StageKey(workflowadapter.TestGen):
		return base + " The content_base64 value must encode the final raw file bytes themselves, never an extra JSON object, artifact-name, format/version, or content-field wrapper."
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		return base + " The content_base64 value must encode exactly one harbor.standard-authoring-tests-analysis.v2 JSON object with version 2, the exact requirement_ids from the validated proposal and plan, and non-empty provided_information, theoretical_path, and passing_evidence fields."
	default:
		return base
	}
}

// outputSchema is sent with turn/start as a first format barrier. It has the
// same closed, stage-derived candidate shape as the tool input, but it is not
// an authority: only a successful tool call can populate accepted.
func (submission *standardAuthoringCodexOutputSubmission) outputSchema() json.RawMessage {
	if submission == nil {
		return nil
	}
	if submission.fixedFileRelativePath != "" {
		return standardAuthoringCodexFixedFileSubmissionSchemaForContract(submission.contractDigest != "")
	}
	return standardAuthoringCodexSubmissionSchemaForContract(submission.stage, submission.contractDigest != "")
}

func standardAuthoringCodexFixedFileSubmissionSchema() json.RawMessage {
	return json.RawMessage(append([]byte(nil), standardAuthoringCodexFixedFileOutputSchemaTemplate()...))
}

func standardAuthoringCodexFixedFileSubmissionSchemaForContract(requireContract bool) json.RawMessage {
	if !requireContract {
		return standardAuthoringCodexFixedFileSubmissionSchema()
	}
	var schema map[string]any
	if err := json.Unmarshal(standardAuthoringCodexFixedFileOutputSchemaTemplate(), &schema); err != nil {
		panic("decode fixed Standard authoring Codex output schema: " + err.Error())
	}
	properties := schema["properties"].(map[string]any)
	properties["contract_digest"] = map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
	schema["required"] = []string{"verdict", "contract_digest"}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("marshal fixed Standard authoring contract submission schema: " + err.Error())
	}
	return encoded
}

func standardAuthoringCodexFixedFileOutputSchemaTemplate() []byte {
	return []byte(standardAuthoringCodexFixedFileOutputSchemaCanonicalJSON)
}

// StandardAuthoringCodexFixedFileOutputSchemaFingerprint identifies the
// exact JSON Schema asset that protects v2 fixed-file solve/test turns.
func StandardAuthoringCodexFixedFileOutputSchemaFingerprint() workflowkit.Fingerprint {
	fingerprint, err := workflowkit.FingerprintBytes("harbor.standard-authoring-codex-fixed-file-submit-schema.v1", standardAuthoringCodexFixedFileOutputSchemaTemplate())
	if err != nil {
		panic("fixed Standard authoring Codex file-submission schema fingerprint: " + err.Error())
	}
	return fingerprint
}

// ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset accepts only the
// exact v2 deployment schema. The optional one terminal LF follows the
// same lock-bound POSIX text-file rule as the legacy Codex output schema.
func ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset(raw []byte) error {
	if len(raw) == 0 || len(raw) > standardAuthoringCodexContractAssetLimit {
		return fmt.Errorf("%w: fixed-file output schema asset has invalid size", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: fixed-file output schema asset has duplicate fields", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	if !json.Valid(raw) || !bytes.Equal(standardAuthoringCodexCanonicalAssetBody(raw), standardAuthoringCodexFixedFileOutputSchemaTemplate()) {
		return fmt.Errorf("%w: fixed-file output schema asset is not the locked JSON Schema template", ErrStandardAuthoringCodexAgentTurnConfiguration)
	}
	return nil
}

// ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage selects the
// one schema that may be used for a frozen v2 template/stage pair.
func ValidateStandardAuthoringCodexOutputSchemaAssetForTemplateStage(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey, raw []byte) error {
	if standardAuthoringCodexUsesFixedFileOutputSchema(template, stageKey) {
		return ValidateStandardAuthoringCodexFixedFileOutputSchemaAsset(raw)
	}
	return ValidateStandardAuthoringCodexOutputSchemaAsset(raw)
}

func StandardAuthoringCodexOutputSchemaFingerprintForTemplateStage(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey) workflowkit.Fingerprint {
	if standardAuthoringCodexUsesFixedFileOutputSchema(template, stageKey) {
		return StandardAuthoringCodexFixedFileOutputSchemaFingerprint()
	}
	return StandardAuthoringCodexOutputSchemaFingerprint()
}

func standardAuthoringCodexUsesFixedFileOutputSchema(template workflowadapter.TemplateReference, stageKey workflowkit.StageKey) bool {
	return template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) && standardAuthoringCodexFixedFileStageKey(stageKey)
}

func standardAuthoringCodexSubmissionSchema(stage workflowkit.StageDescriptor) json.RawMessage {
	return standardAuthoringCodexSubmissionSchemaForContract(stage, false)
}

func standardAuthoringCodexSubmissionSchemaForContract(stage workflowkit.StageDescriptor, requireContract bool) json.RawMessage {
	verdicts := append([]workflowkit.Verdict(nil), stage.Verdicts.Allowed...)
	sort.Slice(verdicts, func(left, right int) bool { return verdicts[left] < verdicts[right] })
	values := make([]string, 0, len(verdicts))
	for _, verdict := range verdicts {
		values = append(values, string(verdict))
	}
	// Start with the same JSON Schema template whose exact bytes are verified
	// from the deployment lock. The only mutations are constraints that cannot
	// live in a shared static asset because they belong to this frozen stage.
	var schema map[string]any
	if err := json.Unmarshal(standardAuthoringCodexOutputSchemaTemplate(), &schema); err != nil {
		panic("decode fixed Standard authoring Codex output schema: " + err.Error())
	}
	properties, propertiesOK := schema["properties"].(map[string]any)
	verdict, verdictOK := properties["verdict"].(map[string]any)
	artifacts, artifactsOK := properties["artifacts"].(map[string]any)
	if !propertiesOK || !verdictOK || !artifactsOK {
		panic("fixed Standard authoring Codex output schema has an invalid shape")
	}
	verdict["enum"] = values
	artifacts["minItems"] = len(stage.Outputs)
	artifacts["maxItems"] = len(stage.Outputs)
	if requireContract {
		properties["contract_digest"] = map[string]any{"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"}
		required, ok := schema["required"].([]any)
		if !ok {
			panic("fixed Standard authoring Codex output schema has no required fields")
		}
		schema["required"] = append(required, "contract_digest")
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("marshal fixed Standard authoring Codex submission schema: " + err.Error())
	}
	return json.RawMessage(encoded)
}

func (submission *standardAuthoringCodexOutputSubmission) handle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	digest := workflowkit.SHA256Fingerprint(raw)
	if submission == nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, 0, digest)
	}

	submission.mu.Lock()
	if submission.accepted != nil {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, remaining, digest)
	}
	if submission.failureCode != "" {
		remaining := submission.remainingLocked()
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, digest)
	}
	if submission.attempts >= submission.maxAttempts {
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submit_attempts_exhausted"}, 0, digest)
	}
	submission.attempts++
	attempt := submission.attempts
	turn := submission.currentTurn
	remaining := submission.remainingLocked()
	submission.mu.Unlock()

	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
	}
	if turn < 1 {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, digest)
	}
	if err := submission.request.Charge(ctx, workflowkit.StageUsage{
		OperationKey: standardAuthoringCodexSubmissionUsageKey(submission.request, turn, attempt),
		Dimension:    standardAuthoringCodexOutputSubmissionQuotaDimension,
		Units:        1,
		OccurredAt:   submission.now().UTC(),
	}); err != nil {
		if contextError(ctx) != nil {
			return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
		}
		failureCode := standardAuthoringCodexSubmissionFailureAccounting
		diagnostic := "submission_accounting_unavailable"
		switch {
		case errors.Is(err, store.ErrQuotaExhausted):
			failureCode = standardAuthoringCodexSubmissionFailureQuota
			diagnostic = "submission_quota_exhausted"
		case errors.Is(err, store.ErrQuotaLeaseExpired), errors.Is(err, store.ErrFencingToken), errors.Is(err, store.ErrLeaseHeld), errors.Is(err, store.ErrImmutable):
			failureCode = standardAuthoringCodexSubmissionFailureLease
			diagnostic = "submission_lease_lost"
		}
		submission.mu.Lock()
		submission.failureCode = failureCode
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{diagnostic}, remaining, digest)
	}
	// invokeDynamicTool returns as soon as the App Server turn context expires,
	// while a handler that was already scheduled may still be unwinding. Do not
	// let a completed quota charge turn into an accepted artifact after that
	// timeout; an expired call is never a submission authority.
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, digest)
	}
	if len(raw) == 0 || len(raw) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, digest)
	}
	if submission.fixedFileRelativePath != "" {
		return submission.handleFixedFileCandidate(ctx, raw, turn, remaining, digest)
	}

	result, canonicalDigest, diagnostic := standardAuthoringCodexValidateSubmissionCandidate(raw, submission.stage, turn, submission.environmentPolicy, submission.contractDigest)
	if diagnostic != "" {
		return standardAuthoringCodexSubmissionResponseWithContract(false, []string{diagnostic}, remaining, digest, submission.contractDigest)
	}
	if result.Outcome.Verdict == workflowkit.VerdictPass {
		if diagnostic := submission.structuredClaimDiagnostic(result); diagnostic != "" {
			return standardAuthoringCodexSubmissionResponseWithContract(false, []string{diagnostic}, remaining, digest, submission.contractDigest)
		}
		if diagnostic := submission.testsAnalysisDiagnostic(result); diagnostic != "" {
			return standardAuthoringCodexSubmissionResponseWithContract(false, []string{diagnostic}, remaining, digest, submission.contractDigest)
		}
	}

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, submission.remainingLocked(), digest)
	}
	// Keep the final cancellation check under the same mutex that protects the
	// sole accepted result. This closes the delayed-handler path where a later
	// turn has started after the original App Server call timed out.
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, submission.remainingLocked(), digest)
	}
	submission.accepted = &standardAuthoringCodexAcceptedOutput{result: cloneStandardAuthoringCodexStageResult(result)}
	return standardAuthoringCodexSubmissionResponseWithContract(true, nil, submission.remainingLocked(), canonicalDigest, submission.contractDigest)
}

// handleFixedFileCandidate admits only a pass receipt for the host-selected
// fixed file. The second safe read closes the gap between structural checking
// and publishing the immutable StageArtifact: an edit after validation must
// be submitted and checked again, never silently become the accepted bytes.
func (submission *standardAuthoringCodexOutputSubmission) handleFixedFileCandidate(ctx context.Context, raw json.RawMessage, turn, remaining int, rawDigest workflowkit.Fingerprint) (json.RawMessage, error) {
	if !standardAuthoringCodexFixedFilePassCandidate(raw, submission.contractDigest) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"wrong_verdict"}, remaining, rawDigest)
	}
	_, outputName, _, ok := standardAuthoringCodexFixedFileStageContract(submission.stage)
	if !ok || submission.fixedFileRelativePath == "" || submission.taskRoot == "" {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexFailureConfiguration
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, rawDigest)
	}
	if submission.readFixedFile == nil {
		submission.mu.Lock()
		submission.failureCode = standardAuthoringCodexFailureConfiguration
		submission.mu.Unlock()
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_unavailable"}, remaining, rawDigest)
	}
	candidate, err := submission.readFixedFile(submission.taskRoot, submission.fixedFileRelativePath, int64(submission.maxBytes))
	if err != nil {
		if errors.Is(err, authoringharness.ErrFixedFileExceedsLimit) {
			return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, rawDigest)
		}
		return standardAuthoringCodexSubmissionResponse(false, []string{"candidate_unavailable"}, remaining, rawDigest)
	}
	if len(candidate) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	artifact := workflowkit.StageArtifact{
		Name: outputName, SchemaVersion: submission.stage.Outputs[0].SchemaVersion, Content: append([]byte(nil), candidate...), TurnOrdinal: turn,
	}
	if contentDiagnostic := standardAuthoringCodexArtifactContentDiagnostic(submission.stage.Key, []workflowkit.StageArtifact{artifact}); contentDiagnostic != "" {
		return standardAuthoringCodexSubmissionResponse(false, []string{contentDiagnostic}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	acceptedContent, err := submission.readFixedFile(submission.taskRoot, submission.fixedFileRelativePath, int64(submission.maxBytes))
	if errors.Is(err, authoringharness.ErrFixedFileExceedsLimit) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if err != nil || !bytes.Equal(candidate, acceptedContent) {
		return standardAuthoringCodexSubmissionResponse(false, []string{"candidate_changed_after_validation"}, remaining, workflowkit.SHA256Fingerprint(candidate))
	}
	if len(acceptedContent) > submission.maxBytes {
		return standardAuthoringCodexSubmissionResponse(false, []string{"byte_limit_exceeded"}, remaining, workflowkit.SHA256Fingerprint(acceptedContent))
	}
	result := workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: workflowkit.VerdictPass},
		Artifacts: []workflowkit.StageArtifact{{
			Name: outputName, SchemaVersion: submission.stage.Outputs[0].SchemaVersion, Content: append([]byte(nil), acceptedContent...), TurnOrdinal: turn,
		}},
	}
	candidateDigest := workflowkit.SHA256Fingerprint(acceptedContent)

	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"already_accepted"}, submission.remainingLocked(), candidateDigest)
	}
	if err := contextError(ctx); err != nil {
		return standardAuthoringCodexSubmissionResponse(false, []string{"submission_timeout"}, submission.remainingLocked(), candidateDigest)
	}
	submission.accepted = &standardAuthoringCodexAcceptedOutput{result: cloneStandardAuthoringCodexStageResult(result)}
	return standardAuthoringCodexSubmissionResponseWithContract(true, nil, submission.remainingLocked(), candidateDigest, submission.contractDigest)
}

type standardAuthoringCodexFixedFileSubmissionCandidate struct {
	Verdict        *workflowkit.Verdict `json:"verdict"`
	ContractDigest *string              `json:"contract_digest"`
}

func standardAuthoringCodexFixedFilePassCandidate(raw []byte, expectedContract workflowkit.Fingerprint) bool {
	if len(raw) == 0 || rejectDuplicateDeploymentCatalogJSONKeys(raw) != nil {
		return false
	}
	var candidate standardAuthoringCodexFixedFileSubmissionCandidate
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	if candidate.Verdict == nil || *candidate.Verdict != workflowkit.VerdictPass {
		return false
	}
	return expectedContract == "" || (candidate.ContractDigest != nil && *candidate.ContractDigest == string(expectedContract))
}

// standardAuthoringCodexFixedFileStageContract is deliberately closed to the
// two pre-harness script producers. It is the only path authority for the
// fixed-file submission mechanism.
func standardAuthoringCodexFixedFileStageContract(stage workflowkit.StageDescriptor) (relative, outputName, diagnostic string, ok bool) {
	switch stage.Key {
	case workflowkit.StageKey(workflowadapter.SolveGen):
		return authoringharness.SolveScriptRelativePath, "solve_script", "solve_script_invalid", true
	case workflowkit.StageKey(workflowadapter.TestGen):
		return authoringharness.TestScriptRelativePath, "test_script", "test_script_invalid", true
	default:
		return "", "", "", false
	}
}

func standardAuthoringCodexFixedFileStageKey(stageKey workflowkit.StageKey) bool {
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.SolveGen), workflowkit.StageKey(workflowadapter.TestGen):
		return true
	default:
		return false
	}
}

func (submission *standardAuthoringCodexOutputSubmission) acceptedResult() (workflowkit.StageExecutionResult, bool) {
	if submission == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	if submission.accepted == nil {
		return workflowkit.StageExecutionResult{}, false
	}
	return cloneStandardAuthoringCodexStageResult(submission.accepted.result), true
}

func (submission *standardAuthoringCodexOutputSubmission) failure() string {
	if submission == nil {
		return ""
	}
	submission.mu.Lock()
	defer submission.mu.Unlock()
	return submission.failureCode
}

func (submission *standardAuthoringCodexOutputSubmission) remainingLocked() int {
	remaining := submission.maxAttempts - submission.attempts
	if remaining < 0 {
		return 0
	}
	return remaining
}

func standardAuthoringCodexSubmissionResponse(accepted bool, diagnostics []string, remaining int, digest workflowkit.Fingerprint) (json.RawMessage, error) {
	return standardAuthoringCodexSubmissionResponseWithContract(accepted, diagnostics, remaining, digest, "")
}

func standardAuthoringCodexSubmissionResponseWithContract(accepted bool, diagnostics []string, remaining int, digest, contractDigest workflowkit.Fingerprint) (json.RawMessage, error) {
	if diagnostics == nil {
		diagnostics = []string{}
	}
	encoded, err := json.Marshal(standardAuthoringCodexSubmissionReceipt{
		Accepted: accepted, Errors: diagnostics, Remaining: remaining, Digest: digest, ContractDigest: contractDigest,
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func standardAuthoringCodexValidateSubmissionCandidate(raw []byte, stage workflowkit.StageDescriptor, turnOrdinal int, environmentPolicy *workflowadapter.StandardAuthoringEnvironmentPolicy, expectedContract workflowkit.Fingerprint) (workflowkit.StageExecutionResult, workflowkit.Fingerprint, string) {
	if turnOrdinal < 1 {
		return workflowkit.StageExecutionResult{}, "", "submission_unavailable"
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	var candidate standardAuthoringCodexSubmissionCandidate
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	if candidate.Verdict == nil || !stage.Verdicts.Allows(*candidate.Verdict) {
		return workflowkit.StageExecutionResult{}, "", "wrong_verdict"
	}
	if expectedContract != "" && (candidate.ContractDigest == nil || *candidate.ContractDigest != string(expectedContract)) {
		return workflowkit.StageExecutionResult{}, "", "contract_digest_mismatch"
	}
	if candidate.Artifacts == nil || len(*candidate.Artifacts) != len(stage.Outputs) {
		return workflowkit.StageExecutionResult{}, "", "artifact_identity_mismatch"
	}

	canonical := standardAuthoringCodexCanonicalSubmission{
		Format:         standardAuthoringCodexCanonicalSubmissionFormat,
		Version:        standardAuthoringCodexCanonicalSubmissionVersion,
		StageKey:       stage.Key,
		StageVersion:   stage.Version,
		Verdict:        *candidate.Verdict,
		ContractDigest: string(expectedContract),
		Artifacts:      make([]standardAuthoringCodexCanonicalSubmissionArtifact, 0, len(*candidate.Artifacts)),
	}
	artifacts := make([]workflowkit.StageArtifact, 0, len(*candidate.Artifacts))
	for index, part := range *candidate.Artifacts {
		if part.ContentBase64 == nil {
			return workflowkit.StageExecutionResult{}, "", "invalid_content_encoding"
		}
		canonicalContentBase64, content, err := canonicalStandardAuthoringCodexBase64(*part.ContentBase64)
		if err != nil {
			return workflowkit.StageExecutionResult{}, "", "invalid_content_encoding"
		}
		specification := stage.Outputs[index]
		canonical.Artifacts = append(canonical.Artifacts, standardAuthoringCodexCanonicalSubmissionArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, ContentBase64: canonicalContentBase64,
		})
		artifacts = append(artifacts, workflowkit.StageArtifact{
			Name: specification.Name, SchemaVersion: specification.SchemaVersion, Content: append([]byte(nil), content...), TurnOrdinal: turnOrdinal,
		})
	}
	if *candidate.Verdict == workflowkit.VerdictPass {
		if stage.Key == workflowkit.StageKey(workflowadapter.DockerfileGen) {
			if environmentPolicy == nil || len(artifacts) != 1 || artifacts[0].Name != "dockerfile" {
				return workflowkit.StageExecutionResult{}, "", "dockerfile_contract_base_image_mismatch"
			}
			if err := workflowadapter.ValidateDockerfileBaseImage(artifacts[0].Content, *environmentPolicy); err != nil {
				return workflowkit.StageExecutionResult{}, "", "dockerfile_contract_base_image_mismatch"
			}
		}
		if diagnostic := standardAuthoringCodexArtifactContentDiagnostic(stage.Key, artifacts); diagnostic != "" {
			return workflowkit.StageExecutionResult{}, "", diagnostic
		}
	}
	canonicalBytes, err := json.Marshal(canonical)
	if err != nil {
		return workflowkit.StageExecutionResult{}, "", "invalid_json"
	}
	return workflowkit.StageExecutionResult{
		Outcome: workflowkit.Outcome{Status: workflowkit.StatusCompleted, Verdict: *candidate.Verdict}, Artifacts: artifacts,
	}, workflowkit.SHA256Fingerprint(canonicalBytes), ""
}

func standardAuthoringCodexArtifactContentDiagnostic(stageKey workflowkit.StageKey, artifacts []workflowkit.StageArtifact) string {
	if len(artifacts) != 1 {
		return ""
	}
	content := artifacts[0].Content
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.InstructionGen):
		if artifacts[0].Name != "instruction" || !standardAuthoringCodexRawInstruction(content) {
			return "instruction_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TaskTOMLGen):
		if artifacts[0].Name != "task_toml" || !standardAuthoringCodexTaskTOML(content) {
			return "task_toml_invalid"
		}
	case workflowkit.StageKey(workflowadapter.SolveGen):
		if artifacts[0].Name != "solve_script" || !standardAuthoringCodexShellScript(content) {
			return "solve_script_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TestGen):
		if artifacts[0].Name != "test_script" || !standardAuthoringCodexShellScript(content) {
			return "test_script_invalid"
		}
	case workflowkit.StageKey(workflowadapter.TestsAnalysis):
		if artifacts[0].Name != "tests_analysis" || !standardAuthoringCodexTestsAnalysis(content, nil) {
			return "tests_analysis_invalid"
		}
	}
	return ""
}

func standardAuthoringCodexRawInstruction(content []byte) bool {
	if !standardAuthoringCodexText(content) {
		return false
	}
	trimmed := bytes.TrimSpace(content)
	if trimmed[0] == '{' || bytes.HasPrefix(trimmed, []byte("```")) {
		return false
	}
	return trimmed[0] != '[' || !json.Valid(trimmed)
}

func standardAuthoringCodexTaskTOML(content []byte) bool {
	return taskpolicy.ValidateStandardAuthoringTaskTOML(content) == nil
}

func standardAuthoringCodexShellScript(content []byte) bool {
	if !standardAuthoringCodexText(content) || !bytes.HasPrefix(content, []byte("#!")) {
		return false
	}
	lineEnd := bytes.IndexByte(content, '\n')
	if lineEnd < 3 || bytes.IndexByte(content[:lineEnd], '\r') >= 0 {
		return false
	}
	return strings.TrimSpace(string(content[lineEnd+1:])) != ""
}

type standardAuthoringCodexTestsAnalysisCandidate struct {
	Format              *string   `json:"format"`
	Version             *string   `json:"version"`
	RequirementIDs      *[]string `json:"requirement_ids"`
	ProvidedInformation *string   `json:"provided_information"`
	TheoreticalPath     *string   `json:"theoretical_path"`
	PassingEvidence     *string   `json:"passing_evidence"`
}

func standardAuthoringCodexTestsAnalysis(content []byte, expectedRequirementIDs map[string]struct{}) bool {
	if len(content) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || rejectDuplicateDeploymentCatalogJSONKeys(content) != nil {
		return false
	}
	var candidate standardAuthoringCodexTestsAnalysisCandidate
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return false
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false
	}
	if candidate.Format == nil || *candidate.Format != standardAuthoringCodexTestsAnalysisFormat || candidate.Version == nil || *candidate.Version != standardAuthoringCodexStructuredClaimsVersion ||
		candidate.RequirementIDs == nil || len(*candidate.RequirementIDs) == 0 {
		return false
	}
	requirementIDs := make(map[string]struct{}, len(*candidate.RequirementIDs))
	for _, id := range *candidate.RequirementIDs {
		if !standardAuthoringCodexRequirementIDPattern.MatchString(id) {
			return false
		}
		if _, duplicate := requirementIDs[id]; duplicate {
			return false
		}
		requirementIDs[id] = struct{}{}
	}
	return (expectedRequirementIDs == nil || standardAuthoringCodexSameRequirementIDs(requirementIDs, expectedRequirementIDs)) &&
		candidate.ProvidedInformation != nil && standardAuthoringCodexNonEmptyText(*candidate.ProvidedInformation) &&
		candidate.TheoreticalPath != nil && standardAuthoringCodexNonEmptyText(*candidate.TheoreticalPath) &&
		candidate.PassingEvidence != nil && standardAuthoringCodexNonEmptyText(*candidate.PassingEvidence)
}

func (submission *standardAuthoringCodexOutputSubmission) testsAnalysisDiagnostic(result workflowkit.StageExecutionResult) string {
	if submission == nil || submission.stage.Key != workflowkit.StageKey(workflowadapter.TestsAnalysis) || len(result.Artifacts) != 1 {
		return ""
	}
	submission.mu.Lock()
	expected := make(map[string]struct{}, len(submission.testsAnalysisRequirementIDs))
	for id := range submission.testsAnalysisRequirementIDs {
		expected[id] = struct{}{}
	}
	contractDigest := submission.contractDigest
	submission.mu.Unlock()
	if contractDigest == "" {
		return ""
	}
	if len(expected) == 0 || !standardAuthoringCodexTestsAnalysis(result.Artifacts[0].Content, expected) {
		return "requirement_ids_invalid"
	}
	return ""
}

func (submission *standardAuthoringCodexOutputSubmission) structuredClaimDiagnostic(result workflowkit.StageExecutionResult) string {
	if submission == nil {
		return "structured_claim_validation_unavailable"
	}
	format, structured := standardAuthoringCodexStructuredClaimFormat(submission.stage)
	if !structured {
		return ""
	}

	submission.mu.Lock()
	var contract *workflowadapter.AuthoringContract
	if submission.structuredClaimContract != nil {
		contractCopy := *submission.structuredClaimContract
		contract = &contractCopy
	}
	sourceRoot := submission.frozenSourceRoot
	contractDigest := submission.contractDigest
	submission.mu.Unlock()
	if contractDigest == "" {
		// A direct legacy fixture can use the generic output route. The v2
		// template always binds a root contract, so it must not take this path.
		return ""
	}
	if contract == nil || sourceRoot == "" {
		return "structured_claim_validation_unavailable"
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].Name != standardAuthoringCodexStructuredClaimOutput(submission.stage.Key) {
		return "structured_claims_invalid"
	}
	return standardAuthoringCodexValidateStructuredClaimDocument(result.Artifacts[0].Content, format, *contract, sourceRoot)
}

func standardAuthoringCodexStructuredClaimFormat(stage workflowkit.StageDescriptor) (string, bool) {
	switch stage.Key {
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return standardAuthoringCodexTaskProposalFormat, true
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return standardAuthoringCodexGeneratedTaskPlanFormat, true
	default:
		return "", false
	}
}

func standardAuthoringCodexStructuredClaimOutput(stageKey workflowkit.StageKey) string {
	switch stageKey {
	case workflowkit.StageKey(workflowadapter.TaskDesign):
		return "task_proposal"
	case workflowkit.StageKey(workflowadapter.GenerateTaskFiles):
		return "generated_task_files"
	default:
		return ""
	}
}

func standardAuthoringCodexValidateStructuredClaimDocument(content []byte, expectedFormat string, contract workflowadapter.AuthoringContract, sourceRoot string) string {
	if len(content) == 0 || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || rejectDuplicateDeploymentCatalogJSONKeys(content) != nil {
		return "structured_claims_invalid"
	}
	var document standardAuthoringCodexStructuredClaimDocument
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return "structured_claims_invalid"
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "structured_claims_invalid"
	}
	if document.Format == nil || *document.Format != expectedFormat || document.Version == nil || *document.Version != standardAuthoringCodexStructuredClaimsVersion {
		return "structured_claims_invalid"
	}
	if !standardAuthoringCodexContractClaimsMatch(document.ContractClaims, contract) {
		return "contract_claims_mismatch"
	}
	if document.Requirements == nil || len(*document.Requirements) == 0 {
		return "requirement_ids_invalid"
	}
	requirementIDs := make(map[string]struct{}, len(*document.Requirements))
	for _, requirement := range *document.Requirements {
		if requirement.ID == nil || !standardAuthoringCodexRequirementIDPattern.MatchString(*requirement.ID) {
			return "requirement_ids_invalid"
		}
		if _, duplicate := requirementIDs[*requirement.ID]; duplicate {
			return "requirement_ids_invalid"
		}
		requirementIDs[*requirement.ID] = struct{}{}
		if requirement.Text == nil || !standardAuthoringCodexNonEmptyText(*requirement.Text) {
			return "structured_claims_invalid"
		}
	}
	if document.SourcePaths == nil {
		return "source_paths_invalid"
	}
	for _, sourcePath := range *document.SourcePaths {
		relative, ok := standardAuthoringCodexSafePOSIXRelativePath(sourcePath, false)
		if !ok || !standardAuthoringCodexFrozenSourcePathExists(sourceRoot, relative, false) {
			return "source_paths_invalid"
		}
	}
	if document.Packages == nil {
		return "packages_invalid"
	}
	for _, packageClaim := range *document.Packages {
		if packageClaim.ManifestPath == nil {
			return "packages_invalid"
		}
		relative, ok := standardAuthoringCodexSafePOSIXRelativePath(*packageClaim.ManifestPath, false)
		if !ok || !standardAuthoringCodexFrozenSourcePathExists(sourceRoot, relative, false) {
			return "packages_invalid"
		}
	}
	if document.Commands == nil {
		return "commands_invalid"
	}
	for _, command := range *document.Commands {
		if command.WorkingDirectory == nil || command.Argv == nil || len(*command.Argv) == 0 {
			return "commands_invalid"
		}
		workingDirectory, ok := standardAuthoringCodexSafePOSIXRelativePath(*command.WorkingDirectory, true)
		if !ok || !standardAuthoringCodexFrozenSourcePathExists(sourceRoot, workingDirectory, true) {
			return "commands_invalid"
		}
		workingDirectoryPath := filepath.Join(sourceRoot, filepath.FromSlash(workingDirectory))
		for _, argument := range *command.Argv {
			if !standardAuthoringCodexNonEmptyCommandArgument(argument) {
				return "commands_invalid"
			}
			if !strings.ContainsAny(argument, "/\\") {
				continue
			}
			relative, ok := standardAuthoringCodexSafePOSIXRelativePath(argument, true)
			if !ok || !standardAuthoringCodexFrozenSourcePathExists(workingDirectoryPath, relative, false) || !standardAuthoringCodexPathAtOrBelow(sourceRoot, filepath.Join(workingDirectoryPath, filepath.FromSlash(relative))) {
				return "commands_invalid"
			}
		}
	}
	return ""
}

func standardAuthoringCodexContractClaimsMatch(claims *standardAuthoringCodexContractClaims, contract workflowadapter.AuthoringContract) bool {
	return claims != nil && claims.Title != nil && *claims.Title == contract.Task.Title &&
		claims.Slug != nil && *claims.Slug == contract.Task.Slug &&
		claims.RepositoryURL != nil && *claims.RepositoryURL == contract.Source.RepositoryURL &&
		claims.CommitSHA != nil && *claims.CommitSHA == contract.Source.CommitSHA &&
		claims.BaseImage != nil && *claims.BaseImage == contract.Environment.BaseImage &&
		claims.CodeLang != nil && *claims.CodeLang == contract.Task.CodeLang &&
		claims.TaskType != nil && *claims.TaskType == contract.Task.TaskType &&
		claims.Application != nil && *claims.Application == contract.Task.Application &&
		claims.Is0To1 != nil && *claims.Is0To1 == contract.Task.Is0To1 &&
		claims.SourceRoot != nil && *claims.SourceRoot == contract.Source.CheckoutRoot
}

func standardAuthoringCodexFrozenSourceRoot(sourceRoot string) (string, error) {
	if strings.TrimSpace(sourceRoot) == "" || sourceRoot != strings.TrimSpace(sourceRoot) {
		return "", errors.New("frozen source root is invalid")
	}
	absolute, err := filepath.Abs(sourceRoot)
	if err != nil || absolute != sourceRoot || filepath.Clean(absolute) != absolute {
		return "", errors.New("frozen source root is invalid")
	}
	info, err := os.Lstat(absolute)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("frozen source root is unavailable or unsafe")
	}
	return absolute, nil
}

func standardAuthoringCodexSafePOSIXRelativePath(value string, allowCurrentDirectory bool) (string, bool) {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\x00") || path.IsAbs(value) {
		return "", false
	}
	for index, component := range strings.Split(value, "/") {
		if component == "" || component == ".." || (component == "." && (!allowCurrentDirectory || index != 0)) {
			return "", false
		}
	}
	cleaned := path.Clean(value)
	if cleaned == "." {
		return ".", allowCurrentDirectory && value == "."
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || (!strings.HasPrefix(value, "./") && cleaned != value) {
		return "", false
	}
	return cleaned, true
}

func standardAuthoringCodexFrozenSourcePathExists(root, relative string, requireDirectory bool) bool {
	candidate := filepath.Join(root, filepath.FromSlash(relative))
	if !standardAuthoringCodexPathAtOrBelow(root, candidate) {
		return false
	}
	info, err := os.Lstat(candidate)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && (!requireDirectory || info.IsDir())
}

func standardAuthoringCodexPathAtOrBelow(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func standardAuthoringCodexNonEmptyCommandArgument(value string) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && !strings.ContainsAny(value, "\r\n\x00")
}

func standardAuthoringCodexText(content []byte) bool {
	return len(bytes.TrimSpace(content)) != 0 && utf8.Valid(content) && bytes.IndexByte(content, 0) < 0
}

func standardAuthoringCodexNonEmptyText(content string) bool {
	return strings.TrimSpace(content) != "" && utf8.ValidString(content) && !strings.ContainsRune(content, '\x00')
}

// canonicalStandardAuthoringCodexBase64 accepts the line-oriented output that
// common shell tooling emits while keeping the stored identity strict. ASCII
// whitespace is transport framing, so it is removed before decoding; the
// decoded bytes must still round-trip to the standard, padded base64 spelling.
func canonicalStandardAuthoringCodexBase64(input string) (string, []byte, error) {
	normalized := make([]byte, 0, len(input))
	for index := 0; index < len(input); index++ {
		switch input[index] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
			continue
		default:
			normalized = append(normalized, input[index])
		}
	}

	content, err := base64.StdEncoding.DecodeString(string(normalized))
	if err != nil {
		return "", nil, err
	}
	canonical := base64.StdEncoding.EncodeToString(content)
	if canonical != string(normalized) {
		return "", nil, errors.New("base64 content is not canonical")
	}
	return canonical, content, nil
}

func cloneStandardAuthoringCodexStageResult(result workflowkit.StageExecutionResult) workflowkit.StageExecutionResult {
	copyResult := result
	copyResult.Artifacts = make([]workflowkit.StageArtifact, len(result.Artifacts))
	for index, artifact := range result.Artifacts {
		copyResult.Artifacts[index] = artifact
		copyResult.Artifacts[index].Content = append([]byte(nil), artifact.Content...)
	}
	return copyResult
}

func standardAuthoringCodexBoundContractDigest(request workflowkit.StageExecutionRequest) (workflowkit.Fingerprint, error) {
	template := workflowadapter.TemplateReference{ID: request.Execution.Workflow.ID, Version: request.Execution.Workflow.Version}
	if !template.Equal(workflowadapter.StandardAuthoringCurrentTemplateReference()) {
		return "", nil
	}
	var digest workflowkit.Fingerprint
	for _, input := range request.Inputs {
		if input.Name != workflowadapter.AuthoringContractArtifact {
			continue
		}
		if digest != "" || input.SchemaVersion != workflowadapter.AuthoringContractSchemaVersion || input.ContentDigest.Validate() != nil {
			return "", errors.New("invalid root contract submission binding")
		}
		digest = input.ContentDigest
	}
	if digest == "" {
		return "", errors.New("missing root contract submission binding")
	}
	return digest, nil
}

func standardAuthoringCodexSubmissionUsageKey(request workflowkit.StageExecutionRequest, turn, attempt int) string {
	return "standard-authoring-codex-output-submission:" + standardAuthoringCodexExecutionKey(request, turn, "submission-"+strconv.Itoa(attempt))
}
