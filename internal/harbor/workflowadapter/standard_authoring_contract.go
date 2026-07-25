package workflowadapter

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// AuthoringContractFormat is the single host-owned immutable source of
	// task direction and source identity for Standard Authoring v2.
	AuthoringContractFormat  = "harbor.standard-authoring-contract.v2"
	AuthoringContractVersion = "2"

	AuthoringContractArtifact          = "authoring_contract"
	AuthoringContractSchemaVersion     = AuthoringContractFormat
	AuthoringContractTitleMaxBytes     = 256
	AuthoringContractObjectiveMaxBytes = 512

	AuthoringContextEnvelopeFormat  = "harbor.standard-authoring-context-envelope.v1"
	AuthoringContextEnvelopeVersion = "1"
)

// AuthoringContract contains only facts frozen by the host. It deliberately
// has no model output, review text, mutable policy map, or filesystem path.
type AuthoringContract struct {
	Format      string                       `json:"format"`
	Version     string                       `json:"version"`
	Task        AuthoringContractTask        `json:"task"`
	Source      AuthoringContractSource      `json:"source"`
	Environment AuthoringContractEnvironment `json:"environment"`
	Objective   string                       `json:"objective"`
	Delivery    AuthoringContractDelivery    `json:"delivery"`
}

type AuthoringContractTask struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	CodeLang    string `json:"code_lang"`
	TaskType    string `json:"task_type"`
	Application string `json:"application"`
	Is0To1      bool   `json:"is_0_to_1"`
}

type AuthoringContractSource struct {
	RepositoryURL  string `json:"repository_url"`
	CommitSHA      string `json:"commit_sha"`
	SnapshotDigest string `json:"snapshot_digest"`
	CheckoutRoot   string `json:"checkout_root"`
}

type AuthoringContractEnvironment struct {
	BaseImage string `json:"base_image"`
}

type AuthoringContractDelivery struct {
	ProfileFingerprint string `json:"profile_fingerprint"`
	PackageFormat      string `json:"package_format"`
}

// NewAuthoringContract returns the canonical root contract after the controlled
// source capture has supplied its content digest. It is intentionally a value
// constructor so callers cannot construct a partially initialized contract.
func NewAuthoringContract(task AuthoringContractTask, source AuthoringContractSource, baseImage, objective, profileFingerprint string) (AuthoringContract, error) {
	return (AuthoringContract{
		Format: AuthoringContractFormat, Version: AuthoringContractVersion,
		Task: task, Source: source,
		Environment: AuthoringContractEnvironment{BaseImage: baseImage},
		Objective:   objective,
		Delivery:    AuthoringContractDelivery{ProfileFingerprint: profileFingerprint, PackageFormat: "codeedge"},
	}).Canonical()
}

func (contract AuthoringContract) Canonical() (AuthoringContract, error) {
	if contract.Format != AuthoringContractFormat || contract.Version != AuthoringContractVersion {
		return AuthoringContract{}, fmt.Errorf("%w: unsupported Standard authoring contract format or version", errInvalidCatalog)
	}
	canonical := contract
	canonical.Task.ID = strings.TrimSpace(contract.Task.ID)
	canonical.Task.Slug = strings.TrimSpace(contract.Task.Slug)
	canonical.Task.Title = strings.TrimSpace(contract.Task.Title)
	canonical.Task.CodeLang = strings.TrimSpace(contract.Task.CodeLang)
	canonical.Task.TaskType = strings.TrimSpace(contract.Task.TaskType)
	canonical.Task.Application = strings.TrimSpace(contract.Task.Application)
	canonical.Source.RepositoryURL = strings.TrimSpace(contract.Source.RepositoryURL)
	canonical.Source.CommitSHA = strings.TrimSpace(contract.Source.CommitSHA)
	canonical.Source.SnapshotDigest = strings.TrimSpace(contract.Source.SnapshotDigest)
	canonical.Source.CheckoutRoot = strings.TrimSpace(contract.Source.CheckoutRoot)
	canonical.Environment.BaseImage = strings.TrimSpace(contract.Environment.BaseImage)
	canonical.Objective = strings.TrimSpace(contract.Objective)
	canonical.Delivery.ProfileFingerprint = strings.TrimSpace(contract.Delivery.ProfileFingerprint)
	canonical.Delivery.PackageFormat = strings.TrimSpace(contract.Delivery.PackageFormat)

	if _, err := uuid.Parse(canonical.Task.ID); err != nil {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract task id: %v", errInvalidCatalog, err)
	}
	if !standardAuthoringContractToken(canonical.Task.Slug) || !standardAuthoringContractToken(canonical.Task.CodeLang) ||
		!standardAuthoringContractToken(canonical.Task.TaskType) || !standardAuthoringContractToken(canonical.Task.Application) {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract task tokens must match [a-z][a-z0-9-]{0,63}", errInvalidCatalog)
	}
	if !utf8.ValidString(canonical.Task.Title) || len(canonical.Task.Title) == 0 || len(canonical.Task.Title) > AuthoringContractTitleMaxBytes || containsControl(canonical.Task.Title) {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract title is invalid", errInvalidCatalog)
	}
	if !utf8.ValidString(canonical.Objective) || len(canonical.Objective) == 0 || len(canonical.Objective) > AuthoringContractObjectiveMaxBytes || containsControl(canonical.Objective) || strings.ContainsAny(canonical.Objective, "\r\n") {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract objective is invalid", errInvalidCatalog)
	}
	if !validAuthoringContractRepositoryURL(canonical.Source.RepositoryURL) || !validAuthoringContractCommit(canonical.Source.CommitSHA) || canonical.Source.CheckoutRoot != "source" {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract source is invalid", errInvalidCatalog)
	}
	if err := workflowkit.Fingerprint(canonical.Source.SnapshotDigest).Validate(); err != nil {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract snapshot digest: %v", errInvalidCatalog, err)
	}
	policy, err := NewStandardAuthoringEnvironmentPolicy(canonical.Environment.BaseImage)
	if err != nil {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract environment: %v", errInvalidCatalog, err)
	}
	canonical.Environment.BaseImage = policy.BaseImage
	if err := workflowkit.Fingerprint(canonical.Delivery.ProfileFingerprint).Validate(); err != nil || canonical.Delivery.PackageFormat != "codeedge" {
		return AuthoringContract{}, fmt.Errorf("%w: Standard authoring contract delivery is invalid", errInvalidCatalog)
	}
	return canonical, nil
}

func (contract AuthoringContract) Validate() error {
	canonical, err := contract.Canonical()
	if err != nil {
		return err
	}
	if canonical != contract {
		return fmt.Errorf("%w: Standard authoring contract is not canonical", errInvalidCatalog)
	}
	return nil
}

func (contract AuthoringContract) CanonicalJSON() ([]byte, error) {
	canonical, err := contract.Canonical()
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func (contract AuthoringContract) ContentDigest() (workflowkit.Fingerprint, error) {
	raw, err := contract.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.SHA256Fingerprint(raw), nil
}

func (contract AuthoringContract) EnvironmentPolicy() (StandardAuthoringEnvironmentPolicy, error) {
	return NewStandardAuthoringEnvironmentPolicy(contract.Environment.BaseImage)
}

func ParseAuthoringContractJSON(raw []byte) (AuthoringContract, error) {
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return AuthoringContract{}, fmt.Errorf("decode Standard authoring contract: %w", err)
	}
	var document authoringContractDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return AuthoringContract{}, fmt.Errorf("decode Standard authoring contract: %w", err)
	}
	return AuthoringContract(document).Canonical()
}

type authoringContractDocument AuthoringContract

func (contract *AuthoringContract) UnmarshalJSON(raw []byte) error {
	if contract == nil {
		return fmt.Errorf("%w: nil Standard authoring contract", errInvalidCatalog)
	}
	parsed, err := ParseAuthoringContractJSON(raw)
	if err != nil {
		return err
	}
	*contract = parsed
	return nil
}

// AuthoringContextEnvelope is the short, host-built model context. Feedback
// identifiers and input references are labelled separately from root facts;
// their contents remain ordinary untrusted artifact data.
type AuthoringContextEnvelope struct {
	Format   string                   `json:"format"`
	Version  string                   `json:"version"`
	Contract AuthoringContextContract `json:"contract"`
	Stage    AuthoringContextStage    `json:"stage"`
	Inputs   []AuthoringContextInput  `json:"inputs"`
	Repairs  []AuthoringContextRepair `json:"repairs,omitempty"`
}

type AuthoringContextContract struct {
	ArtifactID string          `json:"artifact_id"`
	Digest     string          `json:"digest"`
	Content    json.RawMessage `json:"content"`
}

type AuthoringContextStage struct {
	Key     string `json:"key"`
	Attempt int    `json:"attempt"`
}

type AuthoringContextInput struct {
	Name       string `json:"name"`
	ArtifactID string `json:"artifact_id"`
	Digest     string `json:"digest"`
}

type AuthoringContextRepair struct {
	ID             string `json:"id"`
	Target         string `json:"target"`
	Reason         string `json:"reason"`
	State          string `json:"state"`
	EvidenceDigest string `json:"evidence_digest"`
}

func validAuthoringContractRepositoryURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Host != "" && (parsed.Scheme == "https" || parsed.Scheme == "ssh") && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validAuthoringContractCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func standardAuthoringContractToken(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
