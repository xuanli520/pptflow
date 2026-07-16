package workflowadapter

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// StageOperationPayloadKind is the closed discriminator for a provider
// operation payload frozen in a RunExecutionSpec. The payload describes an
// operation invocation, not an unstructured provider configuration bag.
type StageOperationPayloadKind string

const (
	StageOperationPayloadLocalCommand     StageOperationPayloadKind = "local.command"
	StageOperationPayloadContainerCommand StageOperationPayloadKind = "container.command"
	StageOperationPayloadAgentTurn        StageOperationPayloadKind = "agent.turn"
	StageOperationPayloadDurableReview    StageOperationPayloadKind = "durable.review"
	// StageOperationPayloadHarborBuiltin selects a versioned Go-controlled
	// Harbor Flow operation. It is deliberately distinct from local.command:
	// a built-in handler has no ambient executable path or shell contract and
	// must instead be attested against the linked Harbor Flow build by a typed
	// deployment lock.
	StageOperationPayloadHarborBuiltin StageOperationPayloadKind = "harbor.builtin"
)

// StageOperationPayload is sealed to the concrete payloads in this package.
// Callers construct one of the exported value types below; arbitrary maps and
// opaque JSON values cannot enter a frozen execution specification.
type StageOperationPayload interface {
	stageOperationPayload()
	Kind() StageOperationPayloadKind
}

// LocalCommandOperationPayload selects a controlled local command by stable
// registry ID. It is intentionally not a host path and is executed without a
// shell by a provider-owned command registry.
type LocalCommandOperationPayload struct {
	CommandID string   `json:"command_id"`
	Arguments []string `json:"arguments"`
}

// Kind identifies the local command payload variant.
func (LocalCommandOperationPayload) Kind() StageOperationPayloadKind {
	return StageOperationPayloadLocalCommand
}

func (LocalCommandOperationPayload) stageOperationPayload() {}

// ContainerCommandOperationPayload selects an immutable container image and
// an argv command. ImageDigest must be a digest-pinned image reference.
type ContainerCommandOperationPayload struct {
	ImageDigest string   `json:"image_digest"`
	Command     []string `json:"command"`
}

// Kind identifies the container command payload variant.
func (ContainerCommandOperationPayload) Kind() StageOperationPayloadKind {
	return StageOperationPayloadContainerCommand
}

func (ContainerCommandOperationPayload) stageOperationPayload() {}

// AgentReasoningEffort is the closed set of reasoning budgets an agent.turn
// may freeze into a deployment catalog. The executor must never inherit this
// choice from a local Codex configuration.
type AgentReasoningEffort string

const (
	AgentReasoningEffortMinimal AgentReasoningEffort = "minimal"
	AgentReasoningEffortLow     AgentReasoningEffort = "low"
	AgentReasoningEffortMedium  AgentReasoningEffort = "medium"
	AgentReasoningEffortHigh    AgentReasoningEffort = "high"
	AgentReasoningEffortXHigh   AgentReasoningEffort = "xhigh"
)

// Validate accepts an empty value only for decoding historical frozen
// manifests created before reasoning effort was introduced. New controlled
// compositions must require a concrete effort before they can execute.
func (effort AgentReasoningEffort) Validate() error {
	switch effort {
	case "", AgentReasoningEffortMinimal, AgentReasoningEffortLow, AgentReasoningEffortMedium, AgentReasoningEffortHigh, AgentReasoningEffortXHigh:
		return nil
	default:
		return fmt.Errorf("%w: agent reasoning effort %q is unsupported", errInvalidExecutionSpec, effort)
	}
}

// AgentTurnOperationPayload selects an exact controlled agent/model pair,
// reasoning effort, and explicit turn limit. Prompt material is supplied
// through frozen stage inputs, never through an ambient request map.
type AgentTurnOperationPayload struct {
	AgentID         string               `json:"agent_id"`
	ModelID         string               `json:"model_id"`
	ReasoningEffort AgentReasoningEffort `json:"reasoning_effort,omitempty"`
	MaxTurns        int                  `json:"max_turns"`
}

// Kind identifies the agent turn payload variant.
func (AgentTurnOperationPayload) Kind() StageOperationPayloadKind {
	return StageOperationPayloadAgentTurn
}

func (AgentTurnOperationPayload) stageOperationPayload() {}

// DurableReviewOperationPayload identifies the installed durable-review
// policy. It exists because Harbor review gates are nonterminal decisions,
// not command or agent invocations.
type DurableReviewOperationPayload struct {
	PolicyID string `json:"policy_id"`
}

// Kind identifies the durable review payload variant.
func (DurableReviewOperationPayload) Kind() StageOperationPayloadKind {
	return StageOperationPayloadDurableReview
}

func (DurableReviewOperationPayload) stageOperationPayload() {}

// HarborBuiltinOperationPayload selects one exact built-in handler from a
// deployment-owned registry. HandlerID is an immutable capability identity,
// not a Go symbol, package path, shell fragment, or caller-supplied config
// bag. The matching handler version and linker-bound Harbor Flow build belong
// in the deployment operation lock.
type HarborBuiltinOperationPayload struct {
	HandlerID string `json:"handler_id"`
}

// Kind identifies the Harbor Flow built-in operation variant.
func (HarborBuiltinOperationPayload) Kind() StageOperationPayloadKind {
	return StageOperationPayloadHarborBuiltin
}

func (HarborBuiltinOperationPayload) stageOperationPayload() {}

// CloneStageOperationPayload returns an independently owned payload value.
func CloneStageOperationPayload(payload StageOperationPayload) StageOperationPayload {
	switch typed := payload.(type) {
	case LocalCommandOperationPayload:
		if typed.Arguments != nil {
			// An explicit empty argv tail is valid and semantically distinct from
			// a missing array in the strict execution-spec contract.
			typed.Arguments = append([]string{}, typed.Arguments...)
		}
		return typed
	case ContainerCommandOperationPayload:
		if typed.Command != nil {
			typed.Command = append([]string{}, typed.Command...)
		}
		return typed
	case AgentTurnOperationPayload:
		return typed
	case DurableReviewOperationPayload:
		return typed
	case HarborBuiltinOperationPayload:
		return typed
	default:
		return nil
	}
}

// CanonicalStageOperationPayloadJSON validates a sealed payload and encodes a
// deterministic representation with an explicit discriminator.
func CanonicalStageOperationPayloadJSON(payload StageOperationPayload) ([]byte, error) {
	if err := validateStageOperationPayload(payload); err != nil {
		return nil, err
	}
	switch typed := payload.(type) {
	case LocalCommandOperationPayload:
		arguments := append([]string{}, typed.Arguments...)
		return json.Marshal(struct {
			Kind      StageOperationPayloadKind `json:"kind"`
			CommandID string                    `json:"command_id"`
			Arguments []string                  `json:"arguments"`
		}{Kind: typed.Kind(), CommandID: typed.CommandID, Arguments: arguments})
	case ContainerCommandOperationPayload:
		command := append([]string{}, typed.Command...)
		return json.Marshal(struct {
			Kind        StageOperationPayloadKind `json:"kind"`
			ImageDigest string                    `json:"image_digest"`
			Command     []string                  `json:"command"`
		}{Kind: typed.Kind(), ImageDigest: typed.ImageDigest, Command: command})
	case AgentTurnOperationPayload:
		return json.Marshal(struct {
			Kind            StageOperationPayloadKind `json:"kind"`
			AgentID         string                    `json:"agent_id"`
			ModelID         string                    `json:"model_id"`
			ReasoningEffort AgentReasoningEffort      `json:"reasoning_effort,omitempty"`
			MaxTurns        int                       `json:"max_turns"`
		}{Kind: typed.Kind(), AgentID: typed.AgentID, ModelID: typed.ModelID, ReasoningEffort: typed.ReasoningEffort, MaxTurns: typed.MaxTurns})
	case DurableReviewOperationPayload:
		return json.Marshal(struct {
			Kind     StageOperationPayloadKind `json:"kind"`
			PolicyID string                    `json:"policy_id"`
		}{Kind: typed.Kind(), PolicyID: typed.PolicyID})
	case HarborBuiltinOperationPayload:
		return json.Marshal(struct {
			Kind      StageOperationPayloadKind `json:"kind"`
			HandlerID string                    `json:"handler_id"`
		}{Kind: typed.Kind(), HandlerID: typed.HandlerID})
	default:
		return nil, fmt.Errorf("%w: unsupported stage operation payload %T", errInvalidExecutionSpec, payload)
	}
}

// ParseStageOperationPayloadJSON strictly decodes one sealed payload. It
// rejects unknown fields, duplicate keys, unknown variants, and trailing
// values before the payload can be placed in a frozen execution document.
func ParseStageOperationPayloadJSON(raw []byte) (StageOperationPayload, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: stage operation payload is required", errInvalidExecutionSpec)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("%w: stage operation payload: %v", errInvalidExecutionSpec, err)
	}
	var discriminator struct {
		Kind StageOperationPayloadKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return nil, fmt.Errorf("%w: decode stage operation payload discriminator: %v", errInvalidExecutionSpec, err)
	}

	var payload StageOperationPayload
	switch discriminator.Kind {
	case StageOperationPayloadLocalCommand:
		var document struct {
			Kind      StageOperationPayloadKind `json:"kind"`
			CommandID string                    `json:"command_id"`
			Arguments []string                  `json:"arguments"`
		}
		if err := decodeExecutionSpecJSON(raw, &document); err != nil {
			return nil, fmt.Errorf("%w: decode local.command payload: %v", errInvalidExecutionSpec, err)
		}
		payload = LocalCommandOperationPayload{CommandID: document.CommandID, Arguments: document.Arguments}
	case StageOperationPayloadContainerCommand:
		var document struct {
			Kind        StageOperationPayloadKind `json:"kind"`
			ImageDigest string                    `json:"image_digest"`
			Command     []string                  `json:"command"`
		}
		if err := decodeExecutionSpecJSON(raw, &document); err != nil {
			return nil, fmt.Errorf("%w: decode container.command payload: %v", errInvalidExecutionSpec, err)
		}
		payload = ContainerCommandOperationPayload{ImageDigest: document.ImageDigest, Command: document.Command}
	case StageOperationPayloadAgentTurn:
		var document struct {
			Kind            StageOperationPayloadKind `json:"kind"`
			AgentID         string                    `json:"agent_id"`
			ModelID         string                    `json:"model_id"`
			ReasoningEffort AgentReasoningEffort      `json:"reasoning_effort"`
			MaxTurns        int                       `json:"max_turns"`
		}
		if err := decodeExecutionSpecJSON(raw, &document); err != nil {
			return nil, fmt.Errorf("%w: decode agent.turn payload: %v", errInvalidExecutionSpec, err)
		}
		payload = AgentTurnOperationPayload{AgentID: document.AgentID, ModelID: document.ModelID, ReasoningEffort: document.ReasoningEffort, MaxTurns: document.MaxTurns}
	case StageOperationPayloadDurableReview:
		var document struct {
			Kind     StageOperationPayloadKind `json:"kind"`
			PolicyID string                    `json:"policy_id"`
		}
		if err := decodeExecutionSpecJSON(raw, &document); err != nil {
			return nil, fmt.Errorf("%w: decode durable.review payload: %v", errInvalidExecutionSpec, err)
		}
		payload = DurableReviewOperationPayload{PolicyID: document.PolicyID}
	case StageOperationPayloadHarborBuiltin:
		var document struct {
			Kind      StageOperationPayloadKind `json:"kind"`
			HandlerID string                    `json:"handler_id"`
		}
		if err := decodeExecutionSpecJSON(raw, &document); err != nil {
			return nil, fmt.Errorf("%w: decode harbor.builtin payload: %v", errInvalidExecutionSpec, err)
		}
		payload = HarborBuiltinOperationPayload{HandlerID: document.HandlerID}
	default:
		return nil, fmt.Errorf("%w: unsupported stage operation payload kind %q", errInvalidExecutionSpec, discriminator.Kind)
	}
	if err := validateStageOperationPayload(payload); err != nil {
		return nil, err
	}
	return CloneStageOperationPayload(payload), nil
}

func validateStageOperationPayload(payload StageOperationPayload) error {
	switch typed := payload.(type) {
	case LocalCommandOperationPayload:
		if err := validateOperationPayloadToken("local command id", typed.CommandID); err != nil {
			return err
		}
		if typed.Arguments == nil {
			return fmt.Errorf("%w: local command arguments must be an explicit array", errInvalidExecutionSpec)
		}
		for index, argument := range typed.Arguments {
			if err := validateExecutionSpecString(fmt.Sprintf("local command argument %d", index), argument); err != nil {
				return err
			}
		}
		return nil
	case ContainerCommandOperationPayload:
		if err := validateContainerImageDigest(typed.ImageDigest); err != nil {
			return err
		}
		if len(typed.Command) == 0 {
			return fmt.Errorf("%w: container command is required", errInvalidExecutionSpec)
		}
		for index, argument := range typed.Command {
			if err := validateExecutionSpecString(fmt.Sprintf("container command argument %d", index), argument); err != nil {
				return err
			}
		}
		return nil
	case AgentTurnOperationPayload:
		if err := validateOperationPayloadToken("agent id", typed.AgentID); err != nil {
			return err
		}
		if err := validateOperationPayloadToken("model id", typed.ModelID); err != nil {
			return err
		}
		if err := typed.ReasoningEffort.Validate(); err != nil {
			return err
		}
		if typed.MaxTurns < 1 {
			return fmt.Errorf("%w: agent max turns must be positive", errInvalidExecutionSpec)
		}
		return nil
	case DurableReviewOperationPayload:
		return validateOperationPayloadToken("durable review policy id", typed.PolicyID)
	case HarborBuiltinOperationPayload:
		return validateOperationPayloadToken("Harbor built-in handler id", typed.HandlerID)
	default:
		return fmt.Errorf("%w: unsupported stage operation payload %T", errInvalidExecutionSpec, payload)
	}
}

func validateOperationPayloadToken(label, value string) error {
	if err := validateExecutionSpecString(label, value); err != nil {
		return err
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return fmt.Errorf("%w: %s contains unsupported character %q", errInvalidExecutionSpec, label, character)
	}
	return nil
}

func validateContainerImageDigest(value string) error {
	if err := validateExecutionSpecString("container image digest", value); err != nil {
		return err
	}
	separator := strings.LastIndex(value, "@sha256:")
	if separator < 1 {
		return fmt.Errorf("%w: container image digest must be pinned with @sha256", errInvalidExecutionSpec)
	}
	digest := value[separator+len("@sha256:"):]
	if len(digest) != 64 {
		return fmt.Errorf("%w: container image digest must contain 64 hex characters", errInvalidExecutionSpec)
	}
	if strings.ToLower(digest) != digest {
		return fmt.Errorf("%w: container image digest must use lowercase hexadecimal", errInvalidExecutionSpec)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("%w: container image digest is not hexadecimal", errInvalidExecutionSpec)
	}
	return nil
}

type stageOperationBindingDocument struct {
	ProviderID  string          `json:"provider_id"`
	OperationID string          `json:"operation_id"`
	Version     string          `json:"version"`
	Payload     json.RawMessage `json:"payload"`
}

// UnmarshalJSON preserves strict payload decoding even when a StageOperation
// binding is nested inside a sealed stage binding document.
func (binding *StageOperationBinding) UnmarshalJSON(raw []byte) error {
	if binding == nil {
		return fmt.Errorf("%w: nil stage operation binding", errInvalidExecutionSpec)
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: stage operation binding: %v", errInvalidExecutionSpec, err)
	}
	var document stageOperationBindingDocument
	if err := decodeExecutionSpecJSON(raw, &document); err != nil {
		return err
	}
	payload, err := ParseStageOperationPayloadJSON(document.Payload)
	if err != nil {
		return err
	}
	decoded := StageOperationBinding{
		ProviderID:  document.ProviderID,
		OperationID: document.OperationID,
		Version:     document.Version,
		Payload:     payload,
	}
	if err := decoded.validate(); err != nil {
		return err
	}
	*binding = decoded
	return nil
}

// MarshalJSON writes the only canonical payload representation accepted by
// the versioned execution specification.
func (binding StageOperationBinding) MarshalJSON() ([]byte, error) {
	payload, err := CanonicalStageOperationPayloadJSON(binding.Payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(stageOperationBindingDocument{
		ProviderID: binding.ProviderID, OperationID: binding.OperationID, Version: binding.Version, Payload: payload,
	})
}
