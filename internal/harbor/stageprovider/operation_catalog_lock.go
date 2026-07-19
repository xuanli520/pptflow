package stageprovider

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// DeploymentOperationCatalogLockFormat identifies the immutable deployment
	// attestation document. Unlike the operation catalog, it pins the concrete
	// implementation evidence that an approved catalog entry is allowed to use.
	DeploymentOperationCatalogLockFormat = "harbor.operation-catalog.lock.v1"
	// DeploymentOperationCatalogLockVersion is deliberately strict. A schema
	// change must use a new format/version instead of accepting a partial old
	// document as though it were an attestation.
	DeploymentOperationCatalogLockVersion = "3"

	// DeploymentOperationCatalogLockFingerprintDomain separates a lock's
	// content identity from both a catalog fingerprint and ordinary artifact
	// content digests.
	DeploymentOperationCatalogLockFingerprintDomain = "harbor.stageprovider.operation-catalog-lock.v3"

	// StandardAuthoringSSHTransportLockFormat and Version identify the
	// deployment-owned SSH acquisition contract used before a Standard
	// AuthoringSession exists. It is intentionally a top-level template lock:
	// source capture is not a stage operation and cannot borrow an arbitrary
	// local.command record.
	StandardAuthoringSSHTransportLockFormat  = "harbor.standard-authoring.ssh-transport.v1"
	StandardAuthoringSSHTransportLockVersion = "1"
	// StandardAuthoringSSHKnownHostsLockFormat and Version identify the
	// package-owned public host-key allow-list carried by the SSH transport.
	StandardAuthoringSSHKnownHostsLockFormat  = "harbor.standard-authoring.ssh-known-hosts.v1"
	StandardAuthoringSSHKnownHostsLockVersion = "1"

	// StandardAuthoringSSHTransportCommandID and
	// StandardAuthoringSSHWrapperShellCommandID name the two locked
	// executables used solely by the pre-session SSH capture adapter. They are
	// not catalog local.command IDs and therefore cannot be selected by a Run.
	StandardAuthoringSSHTransportCommandID    = "standard-authoring.ssh-transport"
	StandardAuthoringSSHWrapperShellCommandID = "standard-authoring.ssh-wrapper-shell"

	// StandardAuthoringSSHKnownHostsRelativePath is the one package-relative,
	// lock-bound host-key allow-list. A package has no ambient ~/.ssh fallback.
	StandardAuthoringSSHKnownHostsRelativePath = "ssh/known_hosts"
	// StandardAuthoringSSHAgentSocketEnvironment is the only environment name
	// production composition may consult for an optional SSH agent socket.
	// Its value is never serialized into a lock, Run, source, or error.
	StandardAuthoringSSHAgentSocketEnvironment = "HARBOR_FACTORY_STANDARD_AUTHORING_SSH_AUTH_SOCK"
	// StandardAuthoringSSHKnownHostsMaxBytes bounds the static host-key asset
	// before it is parsed or handed to OpenSSH.
	StandardAuthoringSSHKnownHostsMaxBytes = 1 * 1024 * 1024
)

var (
	// ErrInvalidDeploymentOperationCatalogLock marks a malformed, incomplete,
	// unversioned, or ambiguous deployment lock. It is never safe to degrade a
	// bad lock into an allow-all deployment.
	ErrInvalidDeploymentOperationCatalogLock = errors.New("harbor stage provider: invalid deployment operation catalog lock")
	// ErrDeploymentOperationCatalogLockUnavailable marks a worker composition
	// that has no installed immutable catalog/lock verifier.
	ErrDeploymentOperationCatalogLockUnavailable = errors.New("harbor stage provider: deployment operation catalog lock is unavailable")
	// ErrDeploymentOperationCatalogLockDrift marks a lock whose frozen receipt,
	// catalog coordinates, or locked execution identity no longer agrees with
	// the installed allow-list.
	ErrDeploymentOperationCatalogLockDrift = errors.New("harbor stage provider: deployment operation catalog lock drift")
	// ErrDeploymentOperationRuntimeAttestationUnavailable marks an execution
	// path without a runtime attestor. The wrapper rejects before the delegate
	// executor can start an external side effect.
	ErrDeploymentOperationRuntimeAttestationUnavailable = errors.New("harbor stage provider: deployment operation runtime attestation is unavailable")
	// ErrDeploymentOperationRuntimeAttestationFailed marks an attestor that
	// could not prove the current binary/image/agent/build state equals the
	// immutable lock.
	ErrDeploymentOperationRuntimeAttestationFailed = errors.New("harbor stage provider: deployment operation runtime attestation failed")
)

// HarborFlowBuildIdentity pins the executable Harbor Flow build responsible
// for a deployment. Commit and content fingerprint are both required: a
// release label alone is not sufficient evidence that a process was built
// from the approved source.
type HarborFlowBuildIdentity struct {
	Module        string                  `json:"module"`
	Version       string                  `json:"version"`
	Commit        string                  `json:"commit"`
	ContentSHA256 workflowkit.Fingerprint `json:"content_sha256"`
}

// Validate proves the build is a concrete, versioned immutable identity.
func (identity HarborFlowBuildIdentity) Validate() error {
	if err := validateOperationCatalogLockString("Harbor Flow build module", identity.Module); err != nil {
		return err
	}
	if err := validateOperationCatalogLockVersion("Harbor Flow build", identity.Version); err != nil {
		return err
	}
	if err := validateOperationCatalogLockCommit(identity.Commit); err != nil {
		return err
	}
	if err := identity.ContentSHA256.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor Flow build content SHA-256: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// CodeEdgeEvaluatorChildExecutionProfileLock is the complete, deployment-
// owned execution envelope for the closed CodeEdge evaluator child. It is
// intentionally a typed ExecutionProfile rather than a projection of a
// parent Run profile: the evaluator child has its own lifecycle, budget,
// continuation, and candidate-provider limits.
//
// Its JSON representation is workflowadapter.ExecutionProfile's canonical
// human-readable profile document (with duration strings), while the Go
// boundary retains the fully resolved typed value. This makes every budget
// field fingerprint-significant through the enclosing deployment lock without
// admitting an untyped configuration bag.
type CodeEdgeEvaluatorChildExecutionProfileLock struct {
	Profile workflowadapter.ExecutionProfile `json:"-"`
}

// CodeEdgePhase1ExecutionProfileLock is the complete, deployment-owned
// execution envelope for the task-revision CodeEdge Phase-1 parent. It is a
// separate lock field rather than a generic profile option: only the parent
// template may carry it, and a handoff provider never derives it from an
// AuthoringSession, evaluator child, or caller request.
//
// Its JSON representation is workflowadapter.ExecutionProfile's canonical
// document. The enclosing deployment lock therefore makes every budget,
// continuation, and candidate-provider limit fingerprint-significant.
type CodeEdgePhase1ExecutionProfileLock struct {
	Profile workflowadapter.ExecutionProfile `json:"-"`
}

// CodeEdgePhase1PreflightProfileLock freezes the task-contract mapping used
// by the parent built-ins.  It is intentionally distinct from the execution
// profile: metadata paths and protected environment names govern content
// validation, not timing.  Keeping both in the immutable lock prevents a
// package from changing its accepted task shape without changing its lock
// identity and linker binding.
type CodeEdgePhase1PreflightProfileLock struct {
	Profile codeedge.Profile `json:"-"`
}

// Clone returns independently-owned slices so a caller cannot modify the
// installed parent preflight policy through an accessor or a lock copy.
func (profile CodeEdgePhase1PreflightProfileLock) Clone() CodeEdgePhase1PreflightProfileLock {
	profile.Profile.Metadata.CodeLang = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.CodeLang...)
	profile.Profile.Metadata.TaskType = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.TaskType...)
	profile.Profile.Metadata.Application = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.Application...)
	profile.Profile.Metadata.IsZeroToOne = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.IsZeroToOne...)
	profile.Profile.Metadata.GitHubURL = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.GitHubURL...)
	profile.Profile.Metadata.CommitID = append(codeedge.TOMLPath(nil), profile.Profile.Metadata.CommitID...)
	profile.Profile.ProtectedEnvironmentVariables = append([]string(nil), profile.Profile.ProtectedEnvironmentVariables...)
	return profile
}

// Validate proves the parent preflight has every required metadata mapping
// and an explicit non-secret protected-environment allow-list.
func (profile CodeEdgePhase1PreflightProfileLock) Validate() error {
	if err := codeedge.ValidateProfile(profile.Profile); err != nil {
		return fmt.Errorf("%w: CodeEdge Phase-1 preflight profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// PreflightProfile returns a validated defensive copy for the closed parent
// executor.  There is no root-config or caller-provided profile fallback.
func (profile CodeEdgePhase1PreflightProfileLock) PreflightProfile() (codeedge.Profile, error) {
	if err := profile.Validate(); err != nil {
		return codeedge.Profile{}, err
	}
	return profile.Clone().Profile, nil
}

// MarshalJSON canonicalizes the one set-like field before encoding it. TOML
// path order remains meaningful, while protected variable order is not.
func (profile CodeEdgePhase1PreflightProfileLock) MarshalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	canonical := profile.Clone()
	sort.Strings(canonical.Profile.ProtectedEnvironmentVariables)
	encoded, err := json.Marshal(canonical.Profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode CodeEdge Phase-1 preflight profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return encoded, nil
}

// UnmarshalJSON accepts only a strict typed profile. The lock parser also
// performs recursive duplicate-key rejection before this method is called.
func (profile *CodeEdgePhase1PreflightProfileLock) UnmarshalJSON(raw []byte) error {
	if profile == nil {
		return fmt.Errorf("%w: nil CodeEdge Phase-1 preflight profile", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: decode CodeEdge Phase-1 preflight profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	var parsed codeedge.Profile
	if err := decodeDeploymentCatalogJSON(raw, &parsed); err != nil {
		return fmt.Errorf("%w: decode CodeEdge Phase-1 preflight profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	resolved := CodeEdgePhase1PreflightProfileLock{Profile: parsed}
	if err := resolved.Validate(); err != nil {
		return err
	}
	*profile = resolved
	return nil
}

// StandardAuthoringExecutionProfileLock is the complete, deployment-owned
// execution envelope for the Standard authoring template. It is deliberately
// a template-specific lock field: authoring starts cannot accept a caller
// budget, derive a profile from a handoff, or fall back to a process default.
//
// Its JSON representation is workflowadapter.ExecutionProfile's canonical
// document. The enclosing deployment lock therefore makes every stage budget,
// continuation limit, and provider timing value fingerprint-significant.
type StandardAuthoringExecutionProfileLock struct {
	Profile workflowadapter.ExecutionProfile `json:"-"`
}

// Clone returns independently owned profile data.
func (profile StandardAuthoringExecutionProfileLock) Clone() StandardAuthoringExecutionProfileLock {
	profile.Profile = profile.Profile.Clone()
	return profile
}

// Validate proves that the profile is complete for exactly the closed Standard
// authoring template.
func (profile StandardAuthoringExecutionProfileLock) Validate() error {
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(profile.Profile.Template) {
		return fmt.Errorf("%w: Standard authoring execution profile must bind an installed Standard authoring template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := profile.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// ExecutionProfile returns a validated defensive copy suitable for controlled
// Standard composition. It has no caller-profile or default-profile fallback.
func (profile StandardAuthoringExecutionProfileLock) ExecutionProfile() (workflowadapter.ExecutionProfile, error) {
	if err := profile.Validate(); err != nil {
		return workflowadapter.ExecutionProfile{}, err
	}
	return profile.Profile.Clone(), nil
}

// MarshalJSON persists exactly the canonical execution profile document.
func (profile StandardAuthoringExecutionProfileLock) MarshalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return profile.Profile.CanonicalJSON()
}

// UnmarshalJSON accepts only the strict workflowadapter execution profile
// document. The enclosing lock parser rejects duplicate keys before this
// method is reached.
func (profile *StandardAuthoringExecutionProfileLock) UnmarshalJSON(raw []byte) error {
	if profile == nil {
		return fmt.Errorf("%w: nil Standard authoring execution profile", ErrInvalidDeploymentOperationCatalogLock)
	}
	parsed, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: decode Standard authoring execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	resolved := StandardAuthoringExecutionProfileLock{Profile: parsed}
	if err := resolved.Validate(); err != nil {
		return err
	}
	*profile = resolved
	return nil
}

// Clone returns independently owned profile data.
func (profile CodeEdgePhase1ExecutionProfileLock) Clone() CodeEdgePhase1ExecutionProfileLock {
	profile.Profile = profile.Profile.Clone()
	return profile
}

// Validate proves that the profile is complete for exactly the closed parent
// template. ExecutionProfile validation enforces the complete 15-stage
// envelope, so an incomplete parent cannot be admitted through a lock.
func (profile CodeEdgePhase1ExecutionProfileLock) Validate() error {
	if !profile.Profile.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return fmt.Errorf("%w: CodeEdge Phase-1 execution profile must bind %s@%s", ErrInvalidDeploymentOperationCatalogLock, workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	if err := profile.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge Phase-1 execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// ExecutionProfile returns a validated defensive copy suitable for controlled
// composition. It intentionally has no parent-profile fallback.
func (profile CodeEdgePhase1ExecutionProfileLock) ExecutionProfile() (workflowadapter.ExecutionProfile, error) {
	if err := profile.Validate(); err != nil {
		return workflowadapter.ExecutionProfile{}, err
	}
	return profile.Profile.Clone(), nil
}

// MarshalJSON persists exactly the canonical execution profile document.
func (profile CodeEdgePhase1ExecutionProfileLock) MarshalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return profile.Profile.CanonicalJSON()
}

// UnmarshalJSON accepts only the strict workflowadapter execution profile
// document. The enclosing lock parser rejects duplicate keys before this
// method is reached.
func (profile *CodeEdgePhase1ExecutionProfileLock) UnmarshalJSON(raw []byte) error {
	if profile == nil {
		return fmt.Errorf("%w: nil CodeEdge Phase-1 execution profile", ErrInvalidDeploymentOperationCatalogLock)
	}
	parsed, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: decode CodeEdge Phase-1 execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	resolved := CodeEdgePhase1ExecutionProfileLock{Profile: parsed}
	if err := resolved.Validate(); err != nil {
		return err
	}
	*profile = resolved
	return nil
}

// CodeEdgePhase1FinalCompliancePolicyLock pins the complete typed policy that
// the parent Run must freeze into its execution specification. It has no
// arbitrary map or string payload: parser, validation, canonicalization, and
// cloning are delegated to the closed CodeEdge policy contract.
type CodeEdgePhase1FinalCompliancePolicyLock struct {
	Policy codeedge.FinalCompliancePolicy `json:"-"`
}

// Clone returns independently owned policy data.
func (policy CodeEdgePhase1FinalCompliancePolicyLock) Clone() CodeEdgePhase1FinalCompliancePolicyLock {
	policy.Policy = policy.Policy.Clone()
	return policy
}

// Validate proves the policy can govern exactly the parent submission
// contract. The submission report schema is a template-level invariant, not
// a deployment choice that an operator or caller may change.
func (policy CodeEdgePhase1FinalCompliancePolicyLock) Validate() error {
	if err := policy.Policy.Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if policy.Policy.SubmissionReportSchemaVersion != workflowadapter.CodeEdgeSubmissionReportSchemaVersion {
		return fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy submission report schema %q, want %q", ErrInvalidDeploymentOperationCatalogLock, policy.Policy.SubmissionReportSchemaVersion, workflowadapter.CodeEdgeSubmissionReportSchemaVersion)
	}
	return nil
}

// FinalCompliancePolicy returns a validated defensive copy suitable for the
// lock-owned parent definition provider.
func (policy CodeEdgePhase1FinalCompliancePolicyLock) FinalCompliancePolicy() (codeedge.FinalCompliancePolicy, error) {
	if err := policy.Validate(); err != nil {
		return codeedge.FinalCompliancePolicy{}, err
	}
	return policy.Policy.Clone(), nil
}

// MarshalJSON persists the policy's canonical typed representation.
func (policy CodeEdgePhase1FinalCompliancePolicyLock) MarshalJSON() ([]byte, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return policy.Policy.CanonicalJSON()
}

// UnmarshalJSON accepts only a strict full typed policy document. The
// deployment-lock parser's recursive duplicate-key check is intentionally
// repeated for direct decoding callers as well.
func (policy *CodeEdgePhase1FinalCompliancePolicyLock) UnmarshalJSON(raw []byte) error {
	if policy == nil {
		return fmt.Errorf("%w: nil CodeEdge Phase-1 final compliance policy", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return fmt.Errorf("%w: decode CodeEdge Phase-1 final compliance policy: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	var parsed codeedge.FinalCompliancePolicy
	if err := decodeDeploymentCatalogJSON(raw, &parsed); err != nil {
		return fmt.Errorf("%w: decode CodeEdge Phase-1 final compliance policy: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	resolved := CodeEdgePhase1FinalCompliancePolicyLock{Policy: parsed}
	if err := resolved.Validate(); err != nil {
		return err
	}
	*policy = resolved
	return nil
}

// Clone returns independently owned profile data so inspection callers cannot
// mutate the process-installed child execution envelope.
func (profile CodeEdgeEvaluatorChildExecutionProfileLock) Clone() CodeEdgeEvaluatorChildExecutionProfileLock {
	profile.Profile = profile.Profile.Clone()
	return profile
}

// Validate proves that the embedded profile is complete for precisely the
// closed evaluator-child template. The template's own validation enforces
// exact stage coverage and max_attempts=1, so these limits cannot become a
// generic evaluator-stage retry policy.
func (profile CodeEdgeEvaluatorChildExecutionProfileLock) Validate() error {
	if !profile.Profile.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
		return fmt.Errorf("%w: CodeEdge evaluator child execution profile must bind %s@%s", ErrInvalidDeploymentOperationCatalogLock, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion)
	}
	if err := profile.Profile.Validate(); err != nil {
		return fmt.Errorf("%w: CodeEdge evaluator child execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// ExecutionProfile returns a validated defensive copy suitable for controlled
// application composition. It deliberately exposes no parent-derived fallback.
func (profile CodeEdgeEvaluatorChildExecutionProfileLock) ExecutionProfile() (workflowadapter.ExecutionProfile, error) {
	if err := profile.Validate(); err != nil {
		return workflowadapter.ExecutionProfile{}, err
	}
	return profile.Profile.Clone(), nil
}

// MarshalJSON persists exactly the canonical execution-profile document. This
// keeps reviewed lock material readable while the enclosing lock canonicalizer
// still includes every field in its immutable fingerprint.
func (profile CodeEdgeEvaluatorChildExecutionProfileLock) MarshalJSON() ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	return profile.Profile.CanonicalJSON()
}

// UnmarshalJSON accepts only the strict workflowadapter profile document.
// Duplicate-key rejection is also performed by the enclosing deployment-lock
// parser before this method runs.
func (profile *CodeEdgeEvaluatorChildExecutionProfileLock) UnmarshalJSON(raw []byte) error {
	if profile == nil {
		return fmt.Errorf("%w: nil CodeEdge evaluator child execution profile", ErrInvalidDeploymentOperationCatalogLock)
	}
	parsed, err := workflowadapter.ParseExecutionProfileJSON(raw)
	if err != nil {
		return fmt.Errorf("%w: decode CodeEdge evaluator child execution profile: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	resolved := CodeEdgeEvaluatorChildExecutionProfileLock{Profile: parsed}
	if err := resolved.Validate(); err != nil {
		return err
	}
	*profile = resolved
	return nil
}

// LocalExecutableLock records the exact executable selected by a
// local.command payload. AbsolutePath is intentionally an attested deployment
// path rather than a Run input: callers can never replace it with PATH lookup
// or an arbitrary command string.
type LocalExecutableLock struct {
	CommandID     string                  `json:"command_id"`
	AbsolutePath  string                  `json:"absolute_path"`
	Version       string                  `json:"version"`
	ContentSHA256 workflowkit.Fingerprint `json:"content_sha256"`
}

// StandardAuthoringSSHKnownHostsLock pins the one package-relative OpenSSH
// known_hosts allow-list used for Standard source capture. The file contains
// public host keys, never private credentials; its raw bytes are still
// fingerprinted so a package cannot change the allowed host identities after
// its deployment lock has been generated.
type StandardAuthoringSSHKnownHostsLock struct {
	Format        string                  `json:"format"`
	Version       string                  `json:"version"`
	RelativePath  string                  `json:"relative_path"`
	ContentSHA256 workflowkit.Fingerprint `json:"content_sha256"`
}

// Validate proves the known_hosts asset is a versioned, package-relative
// allow-list identity instead of a caller- or environment-selected file.
func (lock StandardAuthoringSSHKnownHostsLock) Validate() error {
	if lock.Format != StandardAuthoringSSHKnownHostsLockFormat {
		return fmt.Errorf("%w: unsupported Standard authoring SSH known_hosts format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != StandardAuthoringSSHKnownHostsLockVersion {
		return fmt.Errorf("%w: unsupported Standard authoring SSH known_hosts version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if lock.RelativePath != StandardAuthoringSSHKnownHostsRelativePath {
		return fmt.Errorf("%w: Standard authoring SSH known_hosts path must be %q", ErrInvalidDeploymentOperationCatalogLock, StandardAuthoringSSHKnownHostsRelativePath)
	}
	if err := lock.ContentSHA256.Validate(); err != nil {
		return fmt.Errorf("%w: Standard authoring SSH known_hosts content SHA-256: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// StandardAuthoringSSHTransportLock pins the complete noninteractive SSH
// transport used by source capture before an AuthoringSource exists. It
// intentionally names a fixed OpenSSH client, a fixed shell used only to
// execute a generated argv-safe wrapper, one immutable known_hosts asset, and
// one optional agent-socket environment reference. It cannot carry a private
// key, password, host pattern, mutable config path, or caller input.
type StandardAuthoringSSHTransportLock struct {
	Format                     string                             `json:"format"`
	Version                    string                             `json:"version"`
	SSHExecutable              LocalExecutableLock                `json:"ssh_executable"`
	WrapperShell               LocalExecutableLock                `json:"wrapper_shell"`
	KnownHosts                 StandardAuthoringSSHKnownHostsLock `json:"known_hosts"`
	AgentSocketEnvironmentName string                             `json:"agent_socket_environment_name"`
}

// Clone returns a value copy. All fields are scalar immutable identities.
func (lock StandardAuthoringSSHTransportLock) Clone() StandardAuthoringSSHTransportLock { return lock }

// Validate proves the pre-session SSH acquisition capability is completely
// deployment-owned. The wrapper shell deliberately has no version probe
// contract because POSIX shells such as dash do not provide a stable
// noninteractive version ABI; its Version is instead required to equal its
// content fingerprint and the runtime rehashes it before writing the wrapper.
func (lock StandardAuthoringSSHTransportLock) Validate() error {
	if lock.Format != StandardAuthoringSSHTransportLockFormat {
		return fmt.Errorf("%w: unsupported Standard authoring SSH transport format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != StandardAuthoringSSHTransportLockVersion {
		return fmt.Errorf("%w: unsupported Standard authoring SSH transport version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if err := validateLocalExecutableLock(lock.SSHExecutable); err != nil {
		return fmt.Errorf("%w: Standard authoring SSH executable: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if lock.SSHExecutable.CommandID != StandardAuthoringSSHTransportCommandID {
		return fmt.Errorf("%w: Standard authoring SSH executable command id must be %q", ErrInvalidDeploymentOperationCatalogLock, StandardAuthoringSSHTransportCommandID)
	}
	if !strings.HasPrefix(lock.SSHExecutable.Version, "OpenSSH_") || strings.ContainsAny(lock.SSHExecutable.Version, " \t\r\n") {
		return fmt.Errorf("%w: Standard authoring SSH executable version must be one OpenSSH identity token", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := validateLocalExecutableLock(lock.WrapperShell); err != nil {
		return fmt.Errorf("%w: Standard authoring SSH wrapper shell: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if lock.WrapperShell.CommandID != StandardAuthoringSSHWrapperShellCommandID {
		return fmt.Errorf("%w: Standard authoring SSH wrapper shell command id must be %q", ErrInvalidDeploymentOperationCatalogLock, StandardAuthoringSSHWrapperShellCommandID)
	}
	if strings.ContainsAny(lock.WrapperShell.AbsolutePath, " \t\r\n") || lock.WrapperShell.Version != string(lock.WrapperShell.ContentSHA256) {
		return fmt.Errorf("%w: Standard authoring SSH wrapper shell must use a shebang-safe content-derived version", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := lock.KnownHosts.Validate(); err != nil {
		return err
	}
	if lock.AgentSocketEnvironmentName != StandardAuthoringSSHAgentSocketEnvironment {
		return fmt.Errorf("%w: Standard authoring SSH agent socket environment must be %q", ErrInvalidDeploymentOperationCatalogLock, StandardAuthoringSSHAgentSocketEnvironment)
	}
	return nil
}

// PinnedContainerRuntimeLock records the exact digest-pinned container image
// and the exact controlled runtime used to launch it. Runtime is duplicated
// here intentionally: it makes an image/runtime pairing visible in the lock
// rather than relying on an ambient container engine default.
type PinnedContainerRuntimeLock struct {
	ImageDigest string                           `json:"image_digest"`
	Runtime     workflowadapter.RuntimeReference `json:"runtime"`
}

// AgentModelLock pins the exact logical agent and model identities. Reasoning
// effort lives only in the frozen operation payload, which is already part of
// the canonical deployment lock and Run definition.
type AgentModelLock struct {
	AgentID      string `json:"agent_id"`
	AgentVersion string `json:"agent_version"`
	ModelID      string `json:"model_id"`
	ModelVersion string `json:"model_version"`
}

// DurableReviewPolicyLock exists for the sealed durable.review payload. It
// keeps review gates versioned as well, while leaving the external-execution
// attestation path focused on actual executable stages.
type DurableReviewPolicyLock struct {
	PolicyID string `json:"policy_id"`
	Version  string `json:"version"`
}

const (
	// HarborFlowBuiltinOperationLockFormat identifies a Go-controlled Harbor
	// Flow operation. It is intentionally separate from local.command: a
	// built-in handler has no executable path, argv, shell, or image identity.
	// Its enclosing deployment lock's HarborFlowBuild binds the implementation
	// that may dispatch it.
	HarborFlowBuiltinOperationLockFormat  = "harbor.flow.builtin-operation.v1"
	HarborFlowBuiltinOperationLockVersion = "1"
)

// HarborFlowBuiltinOperationLock pins the exact deployment-owned handler for
// one harbor.builtin payload. HandlerVersion is independent from the outer
// operation version so a reviewed implementation can evolve its internal
// contract only through a new typed lock revision.
type HarborFlowBuiltinOperationLock struct {
	Format         string `json:"format"`
	Version        string `json:"version"`
	HandlerID      string `json:"handler_id"`
	HandlerVersion string `json:"handler_version"`
}

// Clone returns an independently owned copy of a built-in handler lock.
func (lock HarborFlowBuiltinOperationLock) Clone() HarborFlowBuiltinOperationLock { return lock }

// Validate proves a built-in handler identity is explicit and versioned. It
// deliberately does not accept an executable path, Go symbol, config map, or
// caller-selected module: those would reintroduce an ambient execution path.
func (lock HarborFlowBuiltinOperationLock) Validate() error {
	if lock.Format != HarborFlowBuiltinOperationLockFormat {
		return fmt.Errorf("%w: unsupported Harbor built-in lock format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != HarborFlowBuiltinOperationLockVersion {
		return fmt.Errorf("%w: unsupported Harbor built-in lock version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if err := validateOperationCatalogLockToken("Harbor built-in handler id", lock.HandlerID); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("Harbor built-in handler", lock.HandlerVersion)
}

// DeploymentOperationCatalogLockRecord is one fully pinned catalog entry.
// PromptContentFingerprint and SchemaContentFingerprint are fingerprints of
// the immutable prompt/template and schema bytes respectively. They are
// intentionally not prompt contents, so a lock and a Run manifest cannot
// leak proprietary instructions or secret values.
type DeploymentOperationCatalogLockRecord struct {
	Stage                    DeploymentStageContract                   `json:"stage"`
	Provider                 workflowadapter.ProviderReference         `json:"provider"`
	Operation                workflowadapter.StageOperationBinding     `json:"operation"`
	Runtime                  workflowadapter.RuntimeReference          `json:"runtime"`
	Checkout                 DeploymentCheckoutContract                `json:"checkout"`
	Secrets                  []workflowadapter.SecretReference         `json:"secrets"`
	PromptContentFingerprint workflowkit.Fingerprint                   `json:"prompt_content_fingerprint"`
	SchemaContentFingerprint workflowkit.Fingerprint                   `json:"schema_content_fingerprint"`
	ExecutionKind            workflowadapter.StageOperationPayloadKind `json:"execution_kind"`
	LocalExecutable          *LocalExecutableLock                      `json:"local_executable,omitempty"`
	ContainerRuntime         *PinnedContainerRuntimeLock               `json:"container_runtime,omitempty"`
	AgentModel               *AgentModelLock                           `json:"agent_model,omitempty"`
	// HarborFlowBuiltin is the typed lock for a Go-controlled
	// harbor.builtin operation. It is mutually exclusive with external
	// executable/image/agent/review attestation records.
	HarborFlowBuiltin *HarborFlowBuiltinOperationLock `json:"harbor_flow_builtin,omitempty"`
	// CodexAppServer is an optional, typed runtime extension for agent.turn.
	// It does not replace AgentModel: the latter remains the generic semantic
	// agent/model binding while this field freezes the concrete local Codex App
	// Server implementation. Keeping the extension optional preserves support
	// for other controlled agent runtimes without admitting an untyped config
	// bag to the generic engine.
	CodexAppServer      *CodexAppServerOperationLock  `json:"codex_app_server,omitempty"`
	DurableReviewPolicy *DurableReviewPolicyLock      `json:"durable_review_policy,omitempty"`
	HarborEvaluator     *HarborEvaluatorOperationLock `json:"harbor_evaluator,omitempty"`
	// StandardAuthoringContract binds the deployment-relative prompt and
	// schema assets used by one operation in the closed Standard authoring
	// template. Its raw content identities remain the record's existing
	// PromptContentFingerprint and SchemaContentFingerprint, so there is only
	// one authoritative hash for each asset.
	StandardAuthoringContract *StandardAuthoringContractLock `json:"standard_authoring_contract,omitempty"`
}

// Clone returns an independently owned lock record. Resolver accessors and
// runtime-attestation requests always use this copy so a caller cannot mutate
// a process's installed immutable lock.
func (record DeploymentOperationCatalogLockRecord) Clone() DeploymentOperationCatalogLockRecord {
	record.Operation = record.Operation.Clone()
	record.Secrets = cloneDeploymentSecrets(record.Secrets)
	if record.LocalExecutable != nil {
		local := *record.LocalExecutable
		record.LocalExecutable = &local
	}
	if record.ContainerRuntime != nil {
		container := *record.ContainerRuntime
		record.ContainerRuntime = &container
	}
	if record.AgentModel != nil {
		agent := *record.AgentModel
		record.AgentModel = &agent
	}
	if record.HarborFlowBuiltin != nil {
		builtin := record.HarborFlowBuiltin.Clone()
		record.HarborFlowBuiltin = &builtin
	}
	if record.CodexAppServer != nil {
		codex := record.CodexAppServer.Clone()
		record.CodexAppServer = &codex
	}
	if record.DurableReviewPolicy != nil {
		review := *record.DurableReviewPolicy
		record.DurableReviewPolicy = &review
	}
	if record.HarborEvaluator != nil {
		evaluator := record.HarborEvaluator.Clone()
		record.HarborEvaluator = &evaluator
	}
	if record.StandardAuthoringContract != nil {
		contract := record.StandardAuthoringContract.Clone()
		record.StandardAuthoringContract = &contract
	}
	return record
}

// Validate proves that one record contains exactly one versioned attestation
// kind compatible with its sealed operation payload. It verifies the record
// itself; NewDeploymentOperationCatalogLockResolver additionally verifies it
// against the static DeploymentOperationCatalog registration.
func (record DeploymentOperationCatalogLockRecord) Validate() error {
	if err := validateDeploymentStageContractShape(record.Stage); err != nil {
		return fmt.Errorf("%w: stage: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if err := validateOperationCatalogLockProvider(record.Provider); err != nil {
		return err
	}
	if err := validateOperationBinding(record.Operation); err != nil {
		return fmt.Errorf("%w: operation: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if record.Operation.ProviderID != record.Provider.ID {
		return fmt.Errorf("%w: operation provider %q does not match provider %q", ErrInvalidDeploymentOperationCatalogLock, record.Operation.ProviderID, record.Provider.ID)
	}
	if err := validateOperationCatalogLockVersion("operation", record.Operation.Version); err != nil {
		return err
	}
	if err := validateOperationCatalogLockRuntime(record.Runtime); err != nil {
		return err
	}
	if err := validateDeploymentCheckoutContract(record.Checkout); err != nil {
		return fmt.Errorf("%w: checkout: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if record.Secrets == nil {
		return fmt.Errorf("%w: secret references must be an explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	seenSecrets := make(map[string]workflowadapter.SecretReference, len(record.Secrets))
	for _, secret := range record.Secrets {
		if err := validateOperationCatalogLockSecret(secret); err != nil {
			return err
		}
		if existing, duplicate := seenSecrets[secret.ID]; duplicate {
			if existing != secret {
				return fmt.Errorf("%w: secret %q has conflicting provider/version", ErrInvalidDeploymentOperationCatalogLock, secret.ID)
			}
			return fmt.Errorf("%w: duplicate secret %q", ErrInvalidDeploymentOperationCatalogLock, secret.ID)
		}
		seenSecrets[secret.ID] = secret
	}
	if err := record.PromptContentFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: prompt content fingerprint: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if err := record.SchemaContentFingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: schema content fingerprint: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if record.StandardAuthoringContract != nil {
		if err := record.StandardAuthoringContract.Validate(); err != nil {
			return err
		}
	}
	if record.Operation.Payload == nil {
		return fmt.Errorf("%w: operation payload is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	if record.ExecutionKind != record.Operation.Payload.Kind() {
		return fmt.Errorf("%w: execution kind %q does not match operation payload kind %q", ErrInvalidDeploymentOperationCatalogLock, record.ExecutionKind, record.Operation.Payload.Kind())
	}

	specializations := 0
	if record.LocalExecutable != nil {
		specializations++
	}
	if record.ContainerRuntime != nil {
		specializations++
	}
	if record.AgentModel != nil {
		specializations++
	}
	if record.DurableReviewPolicy != nil {
		specializations++
	}
	if record.HarborFlowBuiltin != nil {
		specializations++
	}
	if specializations != 1 {
		return fmt.Errorf("%w: operation must contain exactly one execution attestation record", ErrInvalidDeploymentOperationCatalogLock)
	}

	switch payload := record.Operation.Payload.(type) {
	case workflowadapter.LocalCommandOperationPayload:
		if record.LocalExecutable == nil || record.ContainerRuntime != nil || record.AgentModel != nil || record.HarborFlowBuiltin != nil || record.CodexAppServer != nil || record.DurableReviewPolicy != nil {
			return fmt.Errorf("%w: local.command requires only local_executable", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := validateLocalExecutableLock(*record.LocalExecutable); err != nil {
			return err
		}
		if record.LocalExecutable.CommandID != payload.CommandID {
			return fmt.Errorf("%w: local executable command id %q does not match payload %q", ErrInvalidDeploymentOperationCatalogLock, record.LocalExecutable.CommandID, payload.CommandID)
		}
		if record.HarborEvaluator != nil {
			if !isHarborEvaluatorCommandID(payload.CommandID) {
				return fmt.Errorf("%w: Harbor evaluator lock requires one approved evaluator command id", ErrInvalidDeploymentOperationCatalogLock)
			}
			if len(payload.Arguments) != 0 {
				return fmt.Errorf("%w: Harbor evaluator local.command must not accept argv", ErrInvalidDeploymentOperationCatalogLock)
			}
			if err := record.HarborEvaluator.Validate(); err != nil {
				return err
			}
			if record.HarborEvaluator.Launcher != *record.LocalExecutable {
				return fmt.Errorf("%w: Harbor evaluator launcher does not match local executable lock", ErrInvalidDeploymentOperationCatalogLock)
			}
			if record.HarborEvaluator.Launcher.CommandID != payload.CommandID {
				return fmt.Errorf("%w: Harbor evaluator launcher command id does not match payload", ErrInvalidDeploymentOperationCatalogLock)
			}
		} else if isHarborEvaluatorCommandID(payload.CommandID) {
			return fmt.Errorf("%w: Harbor evaluator command requires a typed Harbor evaluator lock", ErrInvalidDeploymentOperationCatalogLock)
		}
	case workflowadapter.ContainerCommandOperationPayload:
		if record.ContainerRuntime == nil || record.LocalExecutable != nil || record.AgentModel != nil || record.HarborFlowBuiltin != nil || record.CodexAppServer != nil || record.DurableReviewPolicy != nil || record.HarborEvaluator != nil {
			return fmt.Errorf("%w: container.command requires only container_runtime", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := validatePinnedContainerRuntimeLock(*record.ContainerRuntime, record.Runtime); err != nil {
			return err
		}
		if record.ContainerRuntime.ImageDigest != payload.ImageDigest {
			return fmt.Errorf("%w: container image digest %q does not match payload %q", ErrInvalidDeploymentOperationCatalogLock, record.ContainerRuntime.ImageDigest, payload.ImageDigest)
		}
	case workflowadapter.AgentTurnOperationPayload:
		if record.AgentModel == nil || record.LocalExecutable != nil || record.ContainerRuntime != nil || record.HarborFlowBuiltin != nil || record.DurableReviewPolicy != nil || record.HarborEvaluator != nil {
			return fmt.Errorf("%w: agent.turn requires only agent_model", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := validateAgentModelLock(*record.AgentModel); err != nil {
			return err
		}
		if record.AgentModel.AgentID != payload.AgentID || record.AgentModel.ModelID != payload.ModelID {
			return fmt.Errorf("%w: locked agent/model does not match payload", ErrInvalidDeploymentOperationCatalogLock)
		}
		if record.CodexAppServer != nil {
			if err := record.CodexAppServer.Validate(); err != nil {
				return err
			}
		}
	case workflowadapter.DurableReviewOperationPayload:
		if record.DurableReviewPolicy == nil || record.LocalExecutable != nil || record.ContainerRuntime != nil || record.AgentModel != nil || record.HarborFlowBuiltin != nil || record.CodexAppServer != nil || record.HarborEvaluator != nil {
			return fmt.Errorf("%w: durable.review requires only durable_review_policy", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := validateDurableReviewPolicyLock(*record.DurableReviewPolicy); err != nil {
			return err
		}
		if record.DurableReviewPolicy.PolicyID != payload.PolicyID {
			return fmt.Errorf("%w: locked review policy %q does not match payload %q", ErrInvalidDeploymentOperationCatalogLock, record.DurableReviewPolicy.PolicyID, payload.PolicyID)
		}
	case workflowadapter.HarborBuiltinOperationPayload:
		if record.HarborFlowBuiltin == nil || record.LocalExecutable != nil || record.ContainerRuntime != nil || record.AgentModel != nil || record.CodexAppServer != nil || record.DurableReviewPolicy != nil || record.HarborEvaluator != nil {
			return fmt.Errorf("%w: harbor.builtin requires only harbor_flow_builtin", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := record.HarborFlowBuiltin.Validate(); err != nil {
			return err
		}
		if record.HarborFlowBuiltin.HandlerID != payload.HandlerID {
			return fmt.Errorf("%w: locked Harbor built-in handler %q does not match payload %q", ErrInvalidDeploymentOperationCatalogLock, record.HarborFlowBuiltin.HandlerID, payload.HandlerID)
		}
	default:
		return fmt.Errorf("%w: unsupported operation payload %T", ErrInvalidDeploymentOperationCatalogLock, record.Operation.Payload)
	}
	return nil
}

// DeploymentOperationCatalogLock is the versioned source-controlled lock for
// exactly one DeploymentOperationCatalogReceipt. It holds no secret material,
// mutable paths supplied by a Run, provider defaults, or unpinned image tags.
type DeploymentOperationCatalogLock struct {
	Format                                 string                                      `json:"format"`
	Version                                string                                      `json:"version"`
	LockID                                 string                                      `json:"lock_id"`
	LockVersion                            string                                      `json:"lock_version"`
	CatalogReceipt                         DeploymentOperationCatalogReceipt           `json:"catalog_receipt"`
	HarborFlowBuild                        HarborFlowBuildIdentity                     `json:"harbor_flow_build"`
	StandardAuthoringExecutionProfile      *StandardAuthoringExecutionProfileLock      `json:"standard_authoring_execution_profile,omitempty"`
	StandardAuthoringSSHTransport          *StandardAuthoringSSHTransportLock          `json:"standard_authoring_ssh_transport,omitempty"`
	CodeEdgeEvaluatorChildExecutionProfile *CodeEdgeEvaluatorChildExecutionProfileLock `json:"codeedge_evaluator_child_execution_profile,omitempty"`
	CodeEdgePhase1ExecutionProfile         *CodeEdgePhase1ExecutionProfileLock         `json:"codeedge_phase1_execution_profile,omitempty"`
	CodeEdgePhase1PreflightProfile         *CodeEdgePhase1PreflightProfileLock         `json:"codeedge_phase1_preflight_profile,omitempty"`
	CodeEdgePhase1FinalCompliancePolicy    *CodeEdgePhase1FinalCompliancePolicyLock    `json:"codeedge_phase1_final_compliance_policy,omitempty"`
	Operations                             []DeploymentOperationCatalogLockRecord      `json:"operations"`
}

// Clone returns a deep copy suitable for canonicalization and inspection.
func (lock DeploymentOperationCatalogLock) Clone() DeploymentOperationCatalogLock {
	if lock.StandardAuthoringExecutionProfile != nil {
		profile := lock.StandardAuthoringExecutionProfile.Clone()
		lock.StandardAuthoringExecutionProfile = &profile
	}
	if lock.StandardAuthoringSSHTransport != nil {
		transport := lock.StandardAuthoringSSHTransport.Clone()
		lock.StandardAuthoringSSHTransport = &transport
	}
	if lock.CodeEdgeEvaluatorChildExecutionProfile != nil {
		profile := lock.CodeEdgeEvaluatorChildExecutionProfile.Clone()
		lock.CodeEdgeEvaluatorChildExecutionProfile = &profile
	}
	if lock.CodeEdgePhase1ExecutionProfile != nil {
		profile := lock.CodeEdgePhase1ExecutionProfile.Clone()
		lock.CodeEdgePhase1ExecutionProfile = &profile
	}
	if lock.CodeEdgePhase1PreflightProfile != nil {
		profile := lock.CodeEdgePhase1PreflightProfile.Clone()
		lock.CodeEdgePhase1PreflightProfile = &profile
	}
	if lock.CodeEdgePhase1FinalCompliancePolicy != nil {
		policy := lock.CodeEdgePhase1FinalCompliancePolicy.Clone()
		lock.CodeEdgePhase1FinalCompliancePolicy = &policy
	}
	operations := lock.Operations
	lock.Operations = make([]DeploymentOperationCatalogLockRecord, len(operations))
	for index, operation := range operations {
		lock.Operations[index] = operation.Clone()
	}
	return lock
}

// Validate proves the lock is self-consistent, strict, and versioned. It does
// not know which catalog is installed; that exact inventory comparison occurs
// in NewDeploymentOperationCatalogLockResolver.
func (lock DeploymentOperationCatalogLock) Validate() error {
	if lock.Format != DeploymentOperationCatalogLockFormat {
		return fmt.Errorf("%w: unsupported format %q", ErrInvalidDeploymentOperationCatalogLock, lock.Format)
	}
	if lock.Version != DeploymentOperationCatalogLockVersion {
		return fmt.Errorf("%w: unsupported version %q", ErrInvalidDeploymentOperationCatalogLock, lock.Version)
	}
	if err := validateOperationCatalogLockString("lock id", lock.LockID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockVersion("lock", lock.LockVersion); err != nil {
		return err
	}
	if err := lock.CatalogReceipt.Validate(); err != nil {
		return fmt.Errorf("%w: catalog receipt: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if err := lock.HarborFlowBuild.Validate(); err != nil {
		return err
	}
	if workflowadapter.IsStandardAuthoringWorkflowTemplate(lock.CatalogReceipt.Template) {
		if lock.StandardAuthoringExecutionProfile == nil {
			return fmt.Errorf("%w: Standard authoring execution profile is required", ErrInvalidDeploymentOperationCatalogLock)
		}
		if lock.StandardAuthoringSSHTransport == nil {
			return fmt.Errorf("%w: Standard authoring SSH transport is required", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := lock.StandardAuthoringExecutionProfile.Validate(); err != nil {
			return err
		}
		if err := lock.StandardAuthoringSSHTransport.Validate(); err != nil {
			return err
		}
	} else if lock.StandardAuthoringExecutionProfile != nil || lock.StandardAuthoringSSHTransport != nil {
		return fmt.Errorf("%w: Standard authoring execution profile and SSH transport are only valid for installed Standard authoring templates", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.CodeEdgeEvaluatorChildExecutionProfile != nil {
		if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgeEvaluatorChildTemplateReference()) {
			return fmt.Errorf("%w: CodeEdge evaluator child execution profile is only valid for %s@%s", ErrInvalidDeploymentOperationCatalogLock, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateID, workflowadapter.CodeEdgeEvaluatorChildWorkflowTemplateVersion)
		}
		if err := lock.CodeEdgeEvaluatorChildExecutionProfile.Validate(); err != nil {
			return err
		}
	}
	if lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		if lock.CodeEdgePhase1ExecutionProfile == nil {
			return fmt.Errorf("%w: CodeEdge Phase-1 execution profile is required", ErrInvalidDeploymentOperationCatalogLock)
		}
		if lock.CodeEdgePhase1FinalCompliancePolicy == nil {
			return fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy is required", ErrInvalidDeploymentOperationCatalogLock)
		}
		if lock.CodeEdgePhase1PreflightProfile == nil {
			return fmt.Errorf("%w: CodeEdge Phase-1 preflight profile is required", ErrInvalidDeploymentOperationCatalogLock)
		}
		if err := lock.CodeEdgePhase1ExecutionProfile.Validate(); err != nil {
			return err
		}
		if err := lock.CodeEdgePhase1PreflightProfile.Validate(); err != nil {
			return err
		}
		if err := lock.CodeEdgePhase1FinalCompliancePolicy.Validate(); err != nil {
			return err
		}
	} else if lock.CodeEdgePhase1ExecutionProfile != nil || lock.CodeEdgePhase1PreflightProfile != nil || lock.CodeEdgePhase1FinalCompliancePolicy != nil {
		return fmt.Errorf("%w: CodeEdge Phase-1 execution profile, preflight profile, and final compliance policy are only valid for %s@%s", ErrInvalidDeploymentOperationCatalogLock, workflowadapter.CodeEdgePhase1WorkflowTemplateID, workflowadapter.CodeEdgePhase1WorkflowTemplateVersion)
	}
	if lock.Operations == nil {
		return fmt.Errorf("%w: operations must be an explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	seen := make(map[deploymentOperationCoordinate]struct{}, len(lock.Operations))
	for index, record := range lock.Operations {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("%w: operation %d: %v", ErrInvalidDeploymentOperationCatalogLock, index, err)
		}
		coordinate := deploymentCoordinateForLockRecord(record)
		if _, duplicate := seen[coordinate]; duplicate {
			return fmt.Errorf("%w: duplicate operation %s", ErrInvalidDeploymentOperationCatalogLock, coordinate)
		}
		seen[coordinate] = struct{}{}
	}
	return validateStandardAuthoringLockContract(lock)
}

// StandardAuthoringProfile returns the required complete Standard authoring
// execution envelope. A missing value is a hard deployment error: no CLI,
// handoff, or production composition may provide a substitute budget policy.
func (lock DeploymentOperationCatalogLock) StandardAuthoringProfile() (workflowadapter.ExecutionProfile, error) {
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(lock.CatalogReceipt.Template) {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("%w: Standard authoring profile requires the Standard authoring template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.StandardAuthoringExecutionProfile == nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("%w: Standard authoring execution profile is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	return lock.StandardAuthoringExecutionProfile.ExecutionProfile()
}

// StandardAuthoringSSHTransportLock returns the required pre-session SSH capture
// capability. It is intentionally distinct from stage operation locks because
// Git source acquisition occurs before a Run has a stage attempt.
func (lock DeploymentOperationCatalogLock) StandardAuthoringSSHTransportLock() (StandardAuthoringSSHTransportLock, error) {
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(lock.CatalogReceipt.Template) {
		return StandardAuthoringSSHTransportLock{}, fmt.Errorf("%w: Standard authoring SSH transport requires the Standard authoring template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.StandardAuthoringSSHTransport == nil {
		return StandardAuthoringSSHTransportLock{}, fmt.Errorf("%w: Standard authoring SSH transport is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	if err := lock.StandardAuthoringSSHTransport.Validate(); err != nil {
		return StandardAuthoringSSHTransportLock{}, err
	}
	return lock.StandardAuthoringSSHTransport.Clone(), nil
}

// CodeEdgeEvaluatorChildProfile returns the one complete child-owned profile
// carried by this immutable lock. A missing value is intentionally an error:
// production composition must never borrow a parent profile or invent a
// budget default while launching an external evaluator.
func (lock DeploymentOperationCatalogLock) CodeEdgeEvaluatorChildProfile() (workflowadapter.ExecutionProfile, error) {
	if lock.CodeEdgeEvaluatorChildExecutionProfile == nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("%w: CodeEdge evaluator child execution profile is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	return lock.CodeEdgeEvaluatorChildExecutionProfile.ExecutionProfile()
}

// CodeEdgePhase1Profile returns the required complete parent-owned profile.
// A missing value is a hard deployment error: no caller, Standard handoff, or
// evaluator child may supply a substitute budget envelope.
func (lock DeploymentOperationCatalogLock) CodeEdgePhase1Profile() (workflowadapter.ExecutionProfile, error) {
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("%w: CodeEdge Phase-1 profile requires the parent template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.CodeEdgePhase1ExecutionProfile == nil {
		return workflowadapter.ExecutionProfile{}, fmt.Errorf("%w: CodeEdge Phase-1 execution profile is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	return lock.CodeEdgePhase1ExecutionProfile.ExecutionProfile()
}

// CodeEdgePhase1Preflight returns the complete lock-owned preflight mapping.
// A parent executor is never allowed to derive these task-contract fields
// from a Run, a task snapshot, or mutable process configuration.
func (lock DeploymentOperationCatalogLock) CodeEdgePhase1Preflight() (codeedge.Profile, error) {
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return codeedge.Profile{}, fmt.Errorf("%w: CodeEdge Phase-1 preflight profile requires the parent template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.CodeEdgePhase1PreflightProfile == nil {
		return codeedge.Profile{}, fmt.Errorf("%w: CodeEdge Phase-1 preflight profile is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	return lock.CodeEdgePhase1PreflightProfile.PreflightProfile()
}

// CodeEdgePhase1FinalCompliance returns the complete typed final-compliance
// policy required for the parent Run. It returns a defensive copy so neither
// the caller nor a definition provider can mutate the installed lock.
func (lock DeploymentOperationCatalogLock) CodeEdgePhase1FinalCompliance() (codeedge.FinalCompliancePolicy, error) {
	if !lock.CatalogReceipt.Template.Equal(workflowadapter.CodeEdgePhase1TemplateReference()) {
		return codeedge.FinalCompliancePolicy{}, fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy requires the parent template", ErrInvalidDeploymentOperationCatalogLock)
	}
	if lock.CodeEdgePhase1FinalCompliancePolicy == nil {
		return codeedge.FinalCompliancePolicy{}, fmt.Errorf("%w: CodeEdge Phase-1 final compliance policy is required", ErrInvalidDeploymentOperationCatalogLock)
	}
	return lock.CodeEdgePhase1FinalCompliancePolicy.FinalCompliancePolicy()
}

// CanonicalJSON returns a validated stable lock representation. Operation and
// secret-reference ordering is semantic-set ordering; all content identities
// remain fingerprint-significant.
func (lock DeploymentOperationCatalogLock) CanonicalJSON() ([]byte, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	canonical := lock.Clone()
	for index := range canonical.Operations {
		sort.Slice(canonical.Operations[index].Secrets, func(left, right int) bool {
			return deploymentSecretLess(canonical.Operations[index].Secrets[left], canonical.Operations[index].Secrets[right])
		})
		if canonical.Operations[index].HarborEvaluator != nil {
			evaluator := canonical.Operations[index].HarborEvaluator.Clone()
			evaluator.Contract = evaluator.Contract.canonicalized()
			canonical.Operations[index].HarborEvaluator = &evaluator
		}
	}
	sort.Slice(canonical.Operations, func(left, right int) bool {
		return deploymentCoordinateForLockRecord(canonical.Operations[left]).less(deploymentCoordinateForLockRecord(canonical.Operations[right]))
	})
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: encode canonical lock: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return encoded, nil
}

// Fingerprint returns a domain-separated immutable lock identity.
func (lock DeploymentOperationCatalogLock) Fingerprint() (workflowkit.Fingerprint, error) {
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return workflowkit.FingerprintBytes(DeploymentOperationCatalogLockFingerprintDomain, canonical)
}

// ParseDeploymentOperationCatalogLockJSON strictly decodes a lock document.
// It rejects unknown fields and duplicate keys at every nesting level, null
// operations, trailing JSON values, and all invalid/unversioned records.
func ParseDeploymentOperationCatalogLockJSON(raw []byte) (DeploymentOperationCatalogLock, error) {
	if err := rejectDuplicateDeploymentCatalogJSONKeys(raw); err != nil {
		return DeploymentOperationCatalogLock{}, fmt.Errorf("%w: decode lock: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	var document deploymentOperationCatalogLockDocument
	if err := decodeDeploymentCatalogJSON(raw, &document); err != nil {
		return DeploymentOperationCatalogLock{}, fmt.Errorf("%w: decode lock: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	if document.Operations == nil {
		return DeploymentOperationCatalogLock{}, fmt.Errorf("%w: operations must be an explicit array", ErrInvalidDeploymentOperationCatalogLock)
	}
	lock := DeploymentOperationCatalogLock{
		Format: document.Format, Version: document.Version, LockID: document.LockID, LockVersion: document.LockVersion,
		CatalogReceipt: document.CatalogReceipt, HarborFlowBuild: document.HarborFlowBuild,
		StandardAuthoringExecutionProfile:      document.StandardAuthoringExecutionProfile,
		StandardAuthoringSSHTransport:          document.StandardAuthoringSSHTransport,
		CodeEdgeEvaluatorChildExecutionProfile: document.CodeEdgeEvaluatorChildExecutionProfile,
		CodeEdgePhase1ExecutionProfile:         document.CodeEdgePhase1ExecutionProfile,
		CodeEdgePhase1PreflightProfile:         document.CodeEdgePhase1PreflightProfile,
		CodeEdgePhase1FinalCompliancePolicy:    document.CodeEdgePhase1FinalCompliancePolicy,
		Operations:                             document.Operations,
	}
	if err := lock.Validate(); err != nil {
		return DeploymentOperationCatalogLock{}, err
	}
	return lock.Clone(), nil
}

// UnmarshalJSON keeps direct encoding/json decoding strict as well.
func (lock *DeploymentOperationCatalogLock) UnmarshalJSON(raw []byte) error {
	if lock == nil {
		return fmt.Errorf("%w: nil lock", ErrInvalidDeploymentOperationCatalogLock)
	}
	parsed, err := ParseDeploymentOperationCatalogLockJSON(raw)
	if err != nil {
		return err
	}
	*lock = parsed
	return nil
}

type deploymentOperationCatalogLockDocument struct {
	Format                                 string                                      `json:"format"`
	Version                                string                                      `json:"version"`
	LockID                                 string                                      `json:"lock_id"`
	LockVersion                            string                                      `json:"lock_version"`
	CatalogReceipt                         DeploymentOperationCatalogReceipt           `json:"catalog_receipt"`
	HarborFlowBuild                        HarborFlowBuildIdentity                     `json:"harbor_flow_build"`
	StandardAuthoringExecutionProfile      *StandardAuthoringExecutionProfileLock      `json:"standard_authoring_execution_profile,omitempty"`
	StandardAuthoringSSHTransport          *StandardAuthoringSSHTransportLock          `json:"standard_authoring_ssh_transport,omitempty"`
	CodeEdgeEvaluatorChildExecutionProfile *CodeEdgeEvaluatorChildExecutionProfileLock `json:"codeedge_evaluator_child_execution_profile,omitempty"`
	CodeEdgePhase1ExecutionProfile         *CodeEdgePhase1ExecutionProfileLock         `json:"codeedge_phase1_execution_profile,omitempty"`
	CodeEdgePhase1PreflightProfile         *CodeEdgePhase1PreflightProfileLock         `json:"codeedge_phase1_preflight_profile,omitempty"`
	CodeEdgePhase1FinalCompliancePolicy    *CodeEdgePhase1FinalCompliancePolicyLock    `json:"codeedge_phase1_final_compliance_policy,omitempty"`
	Operations                             []DeploymentOperationCatalogLockRecord      `json:"operations"`
}

// DeploymentOperationCatalogLockIdentity is the small immutable tuple that a
// Run manifest can freeze independently of the full lock bytes.
type DeploymentOperationCatalogLockIdentity struct {
	LockID      string                  `json:"lock_id"`
	LockVersion string                  `json:"lock_version"`
	Fingerprint workflowkit.Fingerprint `json:"fingerprint"`
}

// Validate proves the lock identity is safe to compare with an installed
// resolver without first decoding a complete lock document.
func (identity DeploymentOperationCatalogLockIdentity) Validate() error {
	if err := validateOperationCatalogLockString("lock id", identity.LockID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockVersion("lock", identity.LockVersion); err != nil {
		return err
	}
	if err := identity.Fingerprint.Validate(); err != nil {
		return fmt.Errorf("%w: lock fingerprint: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

// DeploymentOperationCatalogLockVerifier is the read-only static-verification
// boundary. It never accepts registrations or mutable configuration from a
// Run. Callers receive defensive record copies only.
type DeploymentOperationCatalogLockVerifier interface {
	CatalogIdentity() DeploymentOperationCatalogIdentity
	CatalogReceipt() DeploymentOperationCatalogReceipt
	LockIdentity() DeploymentOperationCatalogLockIdentity
	HarborFlowBuild() HarborFlowBuildIdentity
	CanonicalLockJSON() []byte
	VerifyCatalogReceipt(DeploymentOperationCatalogReceipt) error
	VerifyLockIdentity(DeploymentOperationCatalogLockIdentity) error
	VerifyStageOperation(workflowadapter.StageOperationResolution) (DeploymentOperationCatalogLockRecord, error)
}

// DeploymentOperationCatalogLockResolver freezes one complete catalog plus
// one complete lock in memory. The constructor requires a one-to-one exact
// inventory match, so a valid lock cannot silently omit a catalog operation
// or describe an unknown operation.
type DeploymentOperationCatalogLockResolver struct {
	catalog     *DeploymentOperationCatalogResolver
	lock        DeploymentOperationCatalogLock
	fingerprint workflowkit.Fingerprint
	canonical   []byte
	records     map[deploymentOperationCoordinate]DeploymentOperationCatalogLockRecord
}

// NewDeploymentOperationCatalogLockResolver creates the immutable static
// verifier. It does no runtime filesystem, image, provider, or secret access;
// those checks belong to DeploymentOperationRuntimeAttestor at execution.
func NewDeploymentOperationCatalogLockResolver(catalog *DeploymentOperationCatalogResolver, lock DeploymentOperationCatalogLock) (*DeploymentOperationCatalogLockResolver, error) {
	if catalog == nil {
		return nil, ErrDeploymentOperationCatalogLockUnavailable
	}
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	if err := catalog.VerifyReceipt(lock.CatalogReceipt); err != nil {
		return nil, fmt.Errorf("%w: catalog receipt: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	canonical, err := lock.CanonicalJSON()
	if err != nil {
		return nil, err
	}
	fingerprint, err := workflowkit.FingerprintBytes(DeploymentOperationCatalogLockFingerprintDomain, canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: fingerprint lock: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	installed := lock.Clone()
	resolver := &DeploymentOperationCatalogLockResolver{
		catalog:     catalog,
		lock:        installed,
		fingerprint: fingerprint,
		canonical:   append([]byte(nil), canonical...),
		records:     make(map[deploymentOperationCoordinate]DeploymentOperationCatalogLockRecord, len(installed.Operations)),
	}
	for _, record := range installed.Operations {
		coordinate := deploymentCoordinateForLockRecord(record)
		registration, present := catalog.operations[coordinate]
		if !present {
			return nil, fmt.Errorf("%w: lock contains unknown catalog operation %s", ErrDeploymentOperationCatalogLockDrift, coordinate)
		}
		if err := verifyOperationCatalogLockRecordRegistration(record, registration); err != nil {
			return nil, err
		}
		resolver.records[coordinate] = record.Clone()
	}
	if len(resolver.records) != len(catalog.operations) {
		for coordinate := range catalog.operations {
			if _, present := resolver.records[coordinate]; !present {
				return nil, fmt.Errorf("%w: lock is missing catalog operation %s", ErrDeploymentOperationCatalogLockDrift, coordinate)
			}
		}
		return nil, fmt.Errorf("%w: lock/catalog operation inventory differs", ErrDeploymentOperationCatalogLockDrift)
	}
	return resolver, nil
}

// CatalogIdentity exposes the loaded catalog's immutable identity.
func (resolver *DeploymentOperationCatalogLockResolver) CatalogIdentity() DeploymentOperationCatalogIdentity {
	if resolver == nil || resolver.catalog == nil {
		return DeploymentOperationCatalogIdentity{}
	}
	return resolver.catalog.CatalogIdentity()
}

// CatalogReceipt returns the frozen catalog receipt locked to this resolver.
func (resolver *DeploymentOperationCatalogLockResolver) CatalogReceipt() DeploymentOperationCatalogReceipt {
	if resolver == nil {
		return DeploymentOperationCatalogReceipt{}
	}
	return resolver.lock.CatalogReceipt
}

// Lock returns a defensive copy of the installed static lock.
func (resolver *DeploymentOperationCatalogLockResolver) Lock() DeploymentOperationCatalogLock {
	if resolver == nil {
		return DeploymentOperationCatalogLock{}
	}
	return resolver.lock.Clone()
}

// LockIdentity returns the compact immutable identity for this lock.
func (resolver *DeploymentOperationCatalogLockResolver) LockIdentity() DeploymentOperationCatalogLockIdentity {
	if resolver == nil {
		return DeploymentOperationCatalogLockIdentity{}
	}
	return DeploymentOperationCatalogLockIdentity{LockID: resolver.lock.LockID, LockVersion: resolver.lock.LockVersion, Fingerprint: resolver.fingerprint}
}

// HarborFlowBuild returns the immutable build identity that an execution-time
// attestor must compare with the currently running Harbor Flow process.
func (resolver *DeploymentOperationCatalogLockResolver) HarborFlowBuild() HarborFlowBuildIdentity {
	if resolver == nil {
		return HarborFlowBuildIdentity{}
	}
	return resolver.lock.HarborFlowBuild
}

// CanonicalLockJSON returns a defensive copy of the exact canonical lock
// bytes frozen at construction time.
func (resolver *DeploymentOperationCatalogLockResolver) CanonicalLockJSON() []byte {
	if resolver == nil {
		return nil
	}
	return append([]byte(nil), resolver.canonical...)
}

// VerifyCatalogReceipt proves a frozen Run catalog receipt belongs to this
// exact catalog/lock pair.
func (resolver *DeploymentOperationCatalogLockResolver) VerifyCatalogReceipt(receipt DeploymentOperationCatalogReceipt) error {
	if resolver == nil || resolver.catalog == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	if err := resolver.verifyStaticLock(); err != nil {
		return err
	}
	if receipt != resolver.lock.CatalogReceipt {
		return fmt.Errorf("%w: frozen catalog receipt does not match operation lock", ErrDeploymentOperationCatalogLockDrift)
	}
	if err := resolver.catalog.VerifyReceipt(receipt); err != nil {
		return fmt.Errorf("%w: catalog receipt: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	return nil
}

// VerifyLockIdentity proves a compact frozen lock binding belongs to this
// resolver before a worker trusts the installed runtime configuration.
func (resolver *DeploymentOperationCatalogLockResolver) VerifyLockIdentity(identity DeploymentOperationCatalogLockIdentity) error {
	if resolver == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	if err := resolver.verifyStaticLock(); err != nil {
		return err
	}
	if identity != resolver.LockIdentity() {
		return fmt.Errorf("%w: frozen lock %q@%q fingerprint %q does not match loaded lock", ErrDeploymentOperationCatalogLockDrift, identity.LockID, identity.LockVersion, identity.Fingerprint)
	}
	return nil
}

// VerifyStageOperation validates the catalog operation and its matching lock
// record on every call. The attested resolver wrapper calls it once before
// delegate resolution and once immediately before delegate execution.
func (resolver *DeploymentOperationCatalogLockResolver) VerifyStageOperation(resolution workflowadapter.StageOperationResolution) (DeploymentOperationCatalogLockRecord, error) {
	if resolver == nil || resolver.catalog == nil {
		return DeploymentOperationCatalogLockRecord{}, ErrDeploymentOperationCatalogLockUnavailable
	}
	if err := resolver.verifyStaticLock(); err != nil {
		return DeploymentOperationCatalogLockRecord{}, err
	}
	registration, err := resolver.catalog.ResolveStageOperation(resolution)
	if err != nil {
		return DeploymentOperationCatalogLockRecord{}, err
	}
	coordinate := deploymentCoordinateForResolution(resolution)
	record, present := resolver.records[coordinate]
	if !present {
		return DeploymentOperationCatalogLockRecord{}, fmt.Errorf("%w: lock has no record for %s", ErrDeploymentOperationCatalogLockDrift, coordinate)
	}
	if err := verifyOperationCatalogLockRecordRegistration(record, registration); err != nil {
		return DeploymentOperationCatalogLockRecord{}, err
	}
	return record.Clone(), nil
}

// verifyStaticLock checks the invariant that the in-memory static lock still
// has exactly the canonical identity frozen at resolver construction. The
// fields are private and defensive copies are returned, but repeating this
// proof gives the execution boundary a clear, auditable invariant before each
// external side effect.
func (resolver *DeploymentOperationCatalogLockResolver) verifyStaticLock() error {
	if resolver == nil || resolver.catalog == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	if err := resolver.catalog.VerifyReceipt(resolver.lock.CatalogReceipt); err != nil {
		return fmt.Errorf("%w: catalog receipt: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	canonical, err := resolver.lock.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("%w: validate installed lock: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	if !bytes.Equal(canonical, resolver.canonical) {
		return fmt.Errorf("%w: installed lock canonical bytes changed", ErrDeploymentOperationCatalogLockDrift)
	}
	fingerprint, err := workflowkit.FingerprintBytes(DeploymentOperationCatalogLockFingerprintDomain, canonical)
	if err != nil {
		return fmt.Errorf("%w: fingerprint installed lock: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	if fingerprint != resolver.fingerprint {
		return fmt.Errorf("%w: installed lock fingerprint changed", ErrDeploymentOperationCatalogLockDrift)
	}
	return nil
}

// DeploymentOperationRuntimeAttestation is the read-only evidence supplied
// to a runtime attestor just before one external provider executor is called.
// It intentionally carries only identities, hashes, and secret references;
// actual secret material remains behind the controlled secret provider.
type DeploymentOperationRuntimeAttestation struct {
	CatalogReceipt  DeploymentOperationCatalogReceipt
	LockIdentity    DeploymentOperationCatalogLockIdentity
	HarborFlowBuild HarborFlowBuildIdentity
	Record          DeploymentOperationCatalogLockRecord
	Resolution      workflowadapter.StageOperationResolution
}

// DeploymentOperationRuntimeAttestor verifies the current deployment state
// (for example executable contents, image availability, agent/model identity,
// prompt/schema bytes, and build identity) against a static lock. It must not
// execute the operation itself. The wrapper invokes it before every delegate
// ExecuteStage call, including retries.
type DeploymentOperationRuntimeAttestor interface {
	AttestDeploymentOperation(context.Context, DeploymentOperationRuntimeAttestation) error
}

// CatalogLockAttestedWorkflowkitProviderOperationResolver composes a static
// catalog/lock verifier with a normal exact provider resolver. It keeps
// StartRun validation side-effect-free, then wraps the resolved executor so
// runtime attestation is mandatory immediately before external execution.
type CatalogLockAttestedWorkflowkitProviderOperationResolver struct {
	verifier DeploymentOperationCatalogLockVerifier
	delegate WorkflowkitProviderOperationResolver
	attestor DeploymentOperationRuntimeAttestor
}

// NewCatalogLockAttestedWorkflowkitProviderOperationResolver constructs the
// read-only execution boundary. A nil attestor is retained as an explicit
// execution-time denial, which lets pure StartRun preflight remain side-effect
// free while guaranteeing no delegate executor can run without attestation.
func NewCatalogLockAttestedWorkflowkitProviderOperationResolver(verifier DeploymentOperationCatalogLockVerifier, delegate WorkflowkitProviderOperationResolver, attestor DeploymentOperationRuntimeAttestor) (*CatalogLockAttestedWorkflowkitProviderOperationResolver, error) {
	if isNilDeploymentOperationCatalogLockVerifier(verifier) {
		return nil, ErrDeploymentOperationCatalogLockUnavailable
	}
	if isNilWorkflowkitProviderOperationResolver(delegate) {
		return nil, fmt.Errorf("%w: delegate provider resolver is not configured", ErrProviderUnavailable)
	}
	if err := verifier.VerifyLockIdentity(verifier.LockIdentity()); err != nil {
		return nil, fmt.Errorf("verify configured operation catalog lock: %w", err)
	}
	return &CatalogLockAttestedWorkflowkitProviderOperationResolver{verifier: verifier, delegate: delegate, attestor: attestor}, nil
}

// CatalogIdentity forwards the immutable catalog identity for future
// lifecycle composition without exposing mutable lock state.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) CatalogIdentity() DeploymentOperationCatalogIdentity {
	if resolver == nil || resolver.verifier == nil {
		return DeploymentOperationCatalogIdentity{}
	}
	return resolver.verifier.CatalogIdentity()
}

// Receipt forwards the catalog receipt in the shape expected by existing
// catalog-aware lifecycle composition. It does not make this package depend on
// app or cmd code.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) Receipt() DeploymentOperationCatalogReceipt {
	if resolver == nil || resolver.verifier == nil {
		return DeploymentOperationCatalogReceipt{}
	}
	return resolver.verifier.CatalogReceipt()
}

// CanonicalReceiptJSON returns canonical catalog receipt bytes without
// rereading mutable deployment configuration.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) CanonicalReceiptJSON() ([]byte, error) {
	if resolver == nil || resolver.verifier == nil {
		return nil, ErrDeploymentOperationCatalogLockUnavailable
	}
	return resolver.verifier.CatalogReceipt().CanonicalJSON()
}

// VerifyReceipt forwards static catalog receipt verification.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) VerifyReceipt(receipt DeploymentOperationCatalogReceipt) error {
	if resolver == nil || resolver.verifier == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	return resolver.verifier.VerifyCatalogReceipt(receipt)
}

// LockIdentity exposes the static immutable lock identity for a worker that
// also freezes a lock receipt in its Run manifest.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) LockIdentity() DeploymentOperationCatalogLockIdentity {
	if resolver == nil || resolver.verifier == nil {
		return DeploymentOperationCatalogLockIdentity{}
	}
	return resolver.verifier.LockIdentity()
}

// VerifyLockIdentity forwards frozen lock-identity verification without
// exposing the private static verifier. Lifecycle code can therefore freeze a
// lock identity at Run admission and re-verify it in every worker mode.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) VerifyLockIdentity(identity DeploymentOperationCatalogLockIdentity) error {
	if resolver == nil || resolver.verifier == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	return resolver.verifier.VerifyLockIdentity(identity)
}

// ValidateStageOperation proves a frozen operation is accepted by both the
// static catalog/lock and the installed exact provider handler. No runtime
// attestor is called here because StartRun admission must have no side effect.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) ValidateStageOperation(resolution workflowadapter.StageOperationResolution) error {
	if resolver == nil || resolver.verifier == nil {
		return ErrDeploymentOperationCatalogLockUnavailable
	}
	if _, err := resolver.verifier.VerifyStageOperation(resolution); err != nil {
		return err
	}
	if resolver.delegate == nil {
		return fmt.Errorf("%w: delegate provider resolver is not configured", ErrProviderUnavailable)
	}
	return resolver.delegate.ValidateStageOperation(resolution)
}

// ResolveWorkflowkitStageOperation validates the static lock before delegate
// resolution, then returns an executor that repeats the static verification
// and runs the runtime attestor immediately before the delegate executes.
func (resolver *CatalogLockAttestedWorkflowkitProviderOperationResolver) ResolveWorkflowkitStageOperation(resolution workflowadapter.StageOperationResolution) (workflowkit.StageExecutor, error) {
	if resolver == nil || resolver.verifier == nil {
		return nil, ErrDeploymentOperationCatalogLockUnavailable
	}
	if _, err := resolver.verifier.VerifyStageOperation(resolution); err != nil {
		return nil, err
	}
	if isNilDeploymentOperationRuntimeAttestor(resolver.attestor) {
		return nil, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	if resolver.delegate == nil {
		return nil, fmt.Errorf("%w: delegate provider resolver is not configured", ErrProviderUnavailable)
	}
	delegate, err := resolver.delegate.ResolveWorkflowkitStageOperation(resolution)
	if err != nil {
		return nil, err
	}
	if isNilWorkflowkitStageExecutor(delegate) {
		return nil, fmt.Errorf("%w: delegate provider resolver returned a nil executor", ErrStageOperationUnavailable)
	}
	return catalogLockAttestedStageExecutor{
		verifier: resolver.verifier, attestor: resolver.attestor, delegate: delegate,
		resolution: resolution.Clone(),
	}, nil
}

type catalogLockAttestedStageExecutor struct {
	verifier   DeploymentOperationCatalogLockVerifier
	attestor   DeploymentOperationRuntimeAttestor
	delegate   workflowkit.StageExecutor
	resolution workflowadapter.StageOperationResolution
}

func (executor catalogLockAttestedStageExecutor) ExecuteStage(ctx context.Context, request workflowkit.StageExecutionRequest) (workflowkit.StageExecutionResult, error) {
	if request.Stage.Key != executor.resolution.StageKey {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: resolved operation stage %q cannot execute request stage %q", ErrDeploymentOperationCatalogLockDrift, executor.resolution.StageKey, request.Stage.Key)
	}
	record, err := executor.verifier.VerifyStageOperation(executor.resolution)
	if err != nil {
		return workflowkit.StageExecutionResult{}, err
	}
	if isNilDeploymentOperationRuntimeAttestor(executor.attestor) {
		return workflowkit.StageExecutionResult{}, ErrDeploymentOperationRuntimeAttestationUnavailable
	}
	attestation := DeploymentOperationRuntimeAttestation{
		CatalogReceipt: executor.verifier.CatalogReceipt(), LockIdentity: executor.verifier.LockIdentity(),
		HarborFlowBuild: executor.verifier.HarborFlowBuild(),
		Record:          record.Clone(), Resolution: executor.resolution.Clone(),
	}
	if err := executor.attestor.AttestDeploymentOperation(ctx, attestation); err != nil {
		return workflowkit.StageExecutionResult{}, fmt.Errorf("%w: %w", ErrDeploymentOperationRuntimeAttestationFailed, err)
	}
	return executor.delegate.ExecuteStage(ctx, request)
}

func deploymentCoordinateForLockRecord(record DeploymentOperationCatalogLockRecord) deploymentOperationCoordinate {
	return deploymentOperationCoordinate{
		stageKey: record.Stage.Key, providerID: record.Provider.ID, providerVersion: record.Provider.Version,
		operationID: record.Operation.OperationID, operationVersion: record.Operation.Version,
	}
}

func verifyOperationCatalogLockRecordRegistration(record DeploymentOperationCatalogLockRecord, registration DeploymentOperationRegistration) error {
	if record.Stage != registration.Stage {
		return fmt.Errorf("%w: lock stage contract differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if record.Provider != registration.Provider {
		return fmt.Errorf("%w: lock provider differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if record.Runtime != registration.Runtime {
		return fmt.Errorf("%w: lock runtime differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if record.Checkout != registration.Checkout {
		return fmt.Errorf("%w: lock checkout differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if !sameDeploymentSecrets(record.Secrets, registration.Secrets) {
		return fmt.Errorf("%w: lock secret references differ for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	lockedPayload, err := canonicalOperationBindingPayload(record.Operation)
	if err != nil {
		return fmt.Errorf("%w: lock operation payload: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	registeredPayload, err := canonicalOperationBindingPayload(registration.Operation)
	if err != nil {
		return fmt.Errorf("%w: catalog operation payload: %v", ErrDeploymentOperationCatalogLockDrift, err)
	}
	if record.Operation.ProviderID != registration.Operation.ProviderID || record.Operation.OperationID != registration.Operation.OperationID || record.Operation.Version != registration.Operation.Version || !bytes.Equal(lockedPayload, registeredPayload) {
		return fmt.Errorf("%w: lock operation differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if registration.HarborEvaluator == nil && record.HarborEvaluator == nil {
		return nil
	}
	if registration.HarborEvaluator == nil || record.HarborEvaluator == nil {
		return fmt.Errorf("%w: Harbor evaluator contract differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	if !sameHarborEvaluatorContract(record.HarborEvaluator.Contract, *registration.HarborEvaluator) {
		return fmt.Errorf("%w: Harbor evaluator contract differs for %s", ErrDeploymentOperationCatalogLockDrift, deploymentCoordinateForLockRecord(record))
	}
	return nil
}

func validateOperationCatalogLockProvider(reference workflowadapter.ProviderReference) error {
	if err := validateOperationCatalogLockString("provider id", reference.ID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockString("provider kind", reference.Kind); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("provider", reference.Version)
}

func validateOperationCatalogLockRuntime(reference workflowadapter.RuntimeReference) error {
	if err := validateOperationCatalogLockString("runtime id", reference.ID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockString("runtime kind", reference.Kind); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("runtime", reference.Version)
}

func validateOperationCatalogLockSecret(reference workflowadapter.SecretReference) error {
	if err := validateOperationCatalogLockString("secret id", reference.ID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockString("secret provider", reference.Provider); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("secret", reference.Version)
}

func validateLocalExecutableLock(local LocalExecutableLock) error {
	if err := validateOperationCatalogLockString("local executable command id", local.CommandID); err != nil {
		return err
	}
	if !filepath.IsAbs(local.AbsolutePath) || filepath.Clean(local.AbsolutePath) != local.AbsolutePath || local.AbsolutePath == string(filepath.Separator) {
		return fmt.Errorf("%w: local executable absolute path %q is not a clean non-root absolute path", ErrInvalidDeploymentOperationCatalogLock, local.AbsolutePath)
	}
	if err := validateOperationCatalogLockString("local executable absolute path", local.AbsolutePath); err != nil {
		return err
	}
	if err := validateOperationCatalogLockVersion("local executable", local.Version); err != nil {
		return err
	}
	if err := local.ContentSHA256.Validate(); err != nil {
		return fmt.Errorf("%w: local executable content SHA-256: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

func validatePinnedContainerRuntimeLock(container PinnedContainerRuntimeLock, expected workflowadapter.RuntimeReference) error {
	if err := validatePinnedContainerImageDigest(container.ImageDigest); err != nil {
		return err
	}
	if err := validateOperationCatalogLockRuntime(container.Runtime); err != nil {
		return err
	}
	if container.Runtime != expected {
		return fmt.Errorf("%w: container runtime %q@%q does not match locked runtime %q@%q", ErrInvalidDeploymentOperationCatalogLock, container.Runtime.ID, container.Runtime.Version, expected.ID, expected.Version)
	}
	return nil
}

func validateAgentModelLock(agent AgentModelLock) error {
	if err := validateOperationCatalogLockString("agent id", agent.AgentID); err != nil {
		return err
	}
	if err := validateOperationCatalogLockVersion("agent", agent.AgentVersion); err != nil {
		return err
	}
	if err := validateOperationCatalogLockString("model id", agent.ModelID); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("model", agent.ModelVersion)
}

func validateDurableReviewPolicyLock(review DurableReviewPolicyLock) error {
	if err := validateOperationCatalogLockString("durable review policy id", review.PolicyID); err != nil {
		return err
	}
	return validateOperationCatalogLockVersion("durable review policy", review.Version)
}

func validatePinnedContainerImageDigest(value string) error {
	separator := strings.LastIndex(value, "@sha256:")
	if separator < 1 {
		return fmt.Errorf("%w: container image digest must be pinned with @sha256", ErrInvalidDeploymentOperationCatalogLock)
	}
	digest := value[separator+len("@sha256:"):]
	if len(digest) != 64 || strings.ToLower(digest) != digest {
		return fmt.Errorf("%w: container image digest must contain canonical lowercase SHA-256", ErrInvalidDeploymentOperationCatalogLock)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("%w: container image digest is not hexadecimal: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

func validateOperationCatalogLockString(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%w: %s is required", ErrInvalidDeploymentOperationCatalogLock, label)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: %s contains a control character", ErrInvalidDeploymentOperationCatalogLock, label)
		}
	}
	return nil
}

// validateOperationCatalogLockToken admits only stable machine identities for
// typed handler/command names. Human-readable versions and paths continue to
// use validateOperationCatalogLockString; a handler ID must never contain a
// shell fragment, whitespace, slash, or other execution syntax.
func validateOperationCatalogLockToken(label, value string) error {
	if err := validateOperationCatalogLockString(label, value); err != nil {
		return err
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return fmt.Errorf("%w: %s contains unsupported character %q", ErrInvalidDeploymentOperationCatalogLock, label, character)
	}
	return nil
}

func validateOperationCatalogLockVersion(label, value string) error {
	if err := validateOperationCatalogLockString(label+" version", value); err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "latest", "unknown", "unversioned", "dev", "*":
		return fmt.Errorf("%w: %s version %q is not immutable", ErrInvalidDeploymentOperationCatalogLock, label, value)
	}
	return nil
}

func validateOperationCatalogLockCommit(commit string) error {
	if len(commit) != 40 && len(commit) != 64 {
		return fmt.Errorf("%w: Harbor Flow build commit must be a 40- or 64-character lowercase Git object id", ErrInvalidDeploymentOperationCatalogLock)
	}
	if strings.ToLower(commit) != commit {
		return fmt.Errorf("%w: Harbor Flow build commit must use lowercase hexadecimal", ErrInvalidDeploymentOperationCatalogLock)
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return fmt.Errorf("%w: Harbor Flow build commit is not hexadecimal: %v", ErrInvalidDeploymentOperationCatalogLock, err)
	}
	return nil
}

func isNilDeploymentOperationCatalogLockVerifier(verifier DeploymentOperationCatalogLockVerifier) bool {
	return isNilOperationCatalogLockValue(verifier)
}

func isNilWorkflowkitProviderOperationResolver(resolver WorkflowkitProviderOperationResolver) bool {
	return isNilOperationCatalogLockValue(resolver)
}

func isNilDeploymentOperationRuntimeAttestor(attestor DeploymentOperationRuntimeAttestor) bool {
	return isNilOperationCatalogLockValue(attestor)
}

func isNilOperationCatalogLockValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var _ DeploymentOperationCatalogLockVerifier = (*DeploymentOperationCatalogLockResolver)(nil)
var _ WorkflowkitProviderOperationResolver = (*CatalogLockAttestedWorkflowkitProviderOperationResolver)(nil)
var _ workflowadapter.StageOperationResolver = (*CatalogLockAttestedWorkflowkitProviderOperationResolver)(nil)
