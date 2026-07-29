package stageprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

// StandardAuthoringDeploymentAssetBundle is the immutable static deployment
// input accepted by application composition. It deliberately excludes a
// Harbor Flow process identity and all runtime handlers: callers must still
// compare the loaded lock to linker-bound build metadata and inject controlled
// Git/materialization/review handlers before any Run can execute.
type StandardAuthoringDeploymentAssetBundle struct {
	Catalog      *DeploymentOperationCatalogResolver
	Lock         DeploymentOperationCatalogLock
	Verifier     *DeploymentOperationCatalogLockResolver
	Manifest     StandardAuthoringContractAssetManifest
	ContractRoot string
}

// LoadStandardAuthoringDeploymentAssetBundle strictly loads the source
// controlled catalog, generated lock, manifest, and every lock-bound asset.
// It rejects a missing lock, symlinked catalog/lock/asset path, parser drift,
// catalog-lock mismatch, manifest-lock mismatch, and raw prompt/schema hash
// drift before application composition can install a provider. It does not
// read a credential, endpoint, Run input, checkout, or model response.
func LoadStandardAuthoringDeploymentAssetBundle(catalogPath, lockPath, contractRoot string) (*StandardAuthoringDeploymentAssetBundle, error) {
	if err := validateStandardAuthoringContractRoot(contractRoot); err != nil {
		return nil, fmt.Errorf("load Standard authoring contract root: %w", err)
	}
	for label, path := range map[string]string{"catalog": catalogPath, "lock": lockPath} {
		absolute, err := filepath.Abs(strings.TrimSpace(path))
		if err != nil || !standardAuthoringDeploymentPathWithin(contractRoot, absolute) {
			return nil, fmt.Errorf("%w: Standard authoring %s path escapes contract root", ErrDeploymentOperationCatalogLockDrift, label)
		}
	}
	catalogRaw, err := readStandardAuthoringDeploymentFile(catalogPath)
	if err != nil {
		return nil, fmt.Errorf("read Standard authoring catalog: %w", err)
	}
	catalogDocument, err := ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return nil, fmt.Errorf("parse Standard authoring catalog: %w", err)
	}
	catalog, err := NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		return nil, fmt.Errorf("resolve Standard authoring catalog: %w", err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(catalog.Template()) {
		return nil, fmt.Errorf("%w: deployment catalog must bind Standard authoring template", ErrDeploymentOperationCatalogDrift)
	}

	manifestPath := filepath.Join(contractRoot, "contract-assets.v1.json")
	manifestRaw, err := readStandardAuthoringDeploymentFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read Standard authoring asset manifest: %w", err)
	}
	manifest, err := ParseStandardAuthoringContractAssetManifestJSON(manifestRaw)
	if err != nil {
		return nil, fmt.Errorf("parse Standard authoring asset manifest: %w", err)
	}
	if !manifest.Template.Equal(catalog.Template()) {
		return nil, fmt.Errorf("%w: Standard authoring asset manifest template %s@%s does not match catalog template %s@%s", ErrDeploymentOperationCatalogDrift, manifest.Template.ID, manifest.Template.Version, catalog.Template().ID, catalog.Template().Version)
	}

	lockRaw, err := readStandardAuthoringDeploymentFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("read Standard authoring catalog lock: %w", err)
	}
	lock, err := ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		return nil, fmt.Errorf("parse Standard authoring catalog lock: %w", err)
	}
	verifier, err := NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		return nil, fmt.Errorf("bind Standard authoring catalog lock: %w", err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(verifier.CatalogReceipt().Template) {
		return nil, fmt.Errorf("%w: deployment lock receipt must bind Standard authoring template", ErrDeploymentOperationCatalogLockDrift)
	}

	assetsByStage := make(map[string]StandardAuthoringContractAssetManifestEntry, len(manifest.Operations))
	for _, entry := range manifest.Operations {
		assetsByStage[string(entry.StageKey)] = entry.Clone()
	}
	for _, record := range verifier.Lock().Operations {
		entry, present := assetsByStage[string(record.Stage.Key)]
		if !present || record.StandardAuthoringContract == nil {
			return nil, fmt.Errorf("%w: generated Standard authoring lock has no manifest contract for stage %q", ErrDeploymentOperationCatalogLockDrift, record.Stage.Key)
		}
		contract := record.StandardAuthoringContract.Clone()
		if contract.Prompt != entry.Prompt || contract.Schema != entry.Schema || !standardAuthoringContractAdditionalSchemasMatchManifest(contract.AdditionalSchemas, entry.AdditionalSchemas) {
			return nil, fmt.Errorf("%w: generated Standard authoring lock contract differs from manifest for stage %q", ErrDeploymentOperationCatalogLockDrift, record.Stage.Key)
		}
		if _, err := readStandardAuthoringContractAsset(context.Background(), contractRoot, contract.Prompt, record.PromptContentFingerprint); err != nil {
			return nil, fmt.Errorf("verify Standard authoring prompt asset for stage %q: %w", record.Stage.Key, err)
		}
		if _, err := readStandardAuthoringContractAsset(context.Background(), contractRoot, contract.Schema, record.SchemaContentFingerprint); err != nil {
			return nil, fmt.Errorf("verify Standard authoring schema asset for stage %q: %w", record.Stage.Key, err)
		}
		for _, schema := range contract.AdditionalSchemas {
			if _, err := readStandardAuthoringContractAsset(context.Background(), contractRoot, schema.StandardAuthoringContractAssetReference, schema.ContentSHA256); err != nil {
				return nil, fmt.Errorf("verify Standard authoring additional schema asset for stage %q: %w", record.Stage.Key, err)
			}
		}
	}
	sshTransport, err := verifier.Lock().StandardAuthoringSSHTransportLock()
	if err != nil {
		return nil, fmt.Errorf("load Standard authoring SSH transport: %w", err)
	}
	if _, err := readStandardAuthoringSSHKnownHostsAsset(contractRoot, sshTransport.KnownHosts); err != nil {
		return nil, fmt.Errorf("verify Standard authoring SSH known_hosts asset: %w", err)
	}
	return &StandardAuthoringDeploymentAssetBundle{
		Catalog: catalog, Lock: verifier.Lock(), Verifier: verifier, Manifest: manifest.Clone(), ContractRoot: contractRoot,
	}, nil
}

func standardAuthoringContractAdditionalSchemasMatchManifest(locked []StandardAuthoringContractAdditionalSchemaLock, manifest []StandardAuthoringContractAssetReference) bool {
	if len(locked) != len(manifest) {
		return false
	}
	for index := range locked {
		if locked[index].StandardAuthoringContractAssetReference != manifest[index] {
			return false
		}
	}
	return true
}

// ReadStandardAuthoringSSHKnownHostsAsset reads and validates the one
// lock-bound host-key allow-list. It is exported for the source-capture
// adapter's immediate-before-fetch recheck; callers receive only public host
// key bytes, never a credential or a mutable config path.
func ReadStandardAuthoringSSHKnownHostsAsset(contractRoot string, lock StandardAuthoringSSHKnownHostsLock) ([]byte, error) {
	return readStandardAuthoringSSHKnownHostsAsset(contractRoot, lock)
}

func readStandardAuthoringSSHKnownHostsAsset(contractRoot string, lock StandardAuthoringSSHKnownHostsLock) ([]byte, error) {
	if err := lock.Validate(); err != nil {
		return nil, fmt.Errorf("known_hosts asset lock: %w", err)
	}
	if err := validateStandardAuthoringContractRoot(contractRoot); err != nil {
		return nil, err
	}
	assetPath := filepath.Join(contractRoot, filepath.FromSlash(lock.RelativePath))
	if !standardAuthoringDeploymentPathWithin(contractRoot, assetPath) {
		return nil, errors.New("known_hosts asset escapes contract root")
	}
	raw, err := readStandardAuthoringDeploymentFile(assetPath)
	if err != nil {
		return nil, err
	}
	if len(raw) > StandardAuthoringSSHKnownHostsMaxBytes {
		return nil, errors.New("known_hosts asset exceeds the fixed size limit")
	}
	if workflowkit.SHA256Fingerprint(raw) != lock.ContentSHA256 {
		return nil, errors.New("known_hosts asset content fingerprint does not match")
	}
	if err := ValidateStandardAuthoringSSHKnownHostsAsset(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ValidateStandardAuthoringSSHKnownHostsAsset accepts a deliberately narrow
// OpenSSH known_hosts subset: explicit, non-pattern host names with a public
// key. Hashes, wildcard patterns, negations, and markers cannot demonstrate a
// pre-network host allow-list match, so they are intentionally rejected.
func ValidateStandardAuthoringSSHKnownHostsAsset(raw []byte) error {
	if len(raw) == 0 {
		return errors.New("known_hosts asset is empty")
	}
	if len(raw) > StandardAuthoringSSHKnownHostsMaxBytes {
		return errors.New("known_hosts asset exceeds the fixed size limit")
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 4096), StandardAuthoringSSHKnownHostsMaxBytes+1)
	records := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[0], "@") {
			return errors.New("known_hosts asset has an unsupported record")
		}
		for _, host := range strings.Split(fields[0], ",") {
			if !standardAuthoringKnownHostsHostToken(host) {
				return errors.New("known_hosts asset has a non-explicit host entry")
			}
		}
		if err := validateOperationCatalogLockToken("Standard authoring SSH host-key algorithm", fields[1]); err != nil {
			return errors.New("known_hosts asset has an invalid host-key algorithm")
		}
		if _, err := base64.StdEncoding.DecodeString(fields[2]); err != nil {
			return errors.New("known_hosts asset has an invalid public host key")
		}
		records++
	}
	if err := scanner.Err(); err != nil {
		return errors.New("known_hosts asset cannot be read")
	}
	if records == 0 {
		return errors.New("known_hosts asset has no host-key records")
	}
	return nil
}

// StandardAuthoringSSHKnownHostsAllowsHost checks the same narrow parsed
// allow-list before Git can open an SSH connection. A default-port source may
// match either the plain hostname form or OpenSSH's explicit [host]:22 form;
// a non-default port must have its explicit bracketed entry.
func StandardAuthoringSSHKnownHostsAllowsHost(raw []byte, hostname, port string) (bool, error) {
	if err := ValidateStandardAuthoringSSHKnownHostsAsset(raw); err != nil {
		return false, err
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	port = strings.TrimSpace(port)
	if hostname == "" || strings.ContainsAny(hostname, " \t\r\n\x00,@!|*?") {
		return false, errors.New("SSH source host is invalid")
	}
	candidates := make(map[string]struct{}, 2)
	if port == "" {
		candidates[hostname] = struct{}{}
	} else {
		parsedPort, err := strconv.ParseUint(port, 10, 16)
		if err != nil || parsedPort == 0 {
			return false, errors.New("SSH source port is invalid")
		}
		canonicalPort := strconv.FormatUint(parsedPort, 10)
		if parsedPort == 22 {
			candidates[hostname] = struct{}{}
			candidates["["+hostname+"]:"+canonicalPort] = struct{}{}
		} else {
			candidates["["+hostname+"]:"+canonicalPort] = struct{}{}
		}
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		fields := strings.Fields(string(line))
		if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		for _, host := range strings.Split(fields[0], ",") {
			if _, found := candidates[strings.ToLower(host)]; found {
				return true, nil
			}
		}
	}
	return false, nil
}

func standardAuthoringKnownHostsHostToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, " \t\r\n\x00,/@!|*?") {
		return false
	}
	if strings.HasPrefix(value, "[") {
		closing := strings.LastIndex(value, "]:")
		if closing <= 1 || closing+2 >= len(value) {
			return false
		}
		for _, character := range value[closing+2:] {
			if character < '0' || character > '9' {
				return false
			}
		}
	}
	return true
}

func readStandardAuthoringDeploymentFile(path string) ([]byte, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("deployment file path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil || filepath.Clean(absolute) != absolute {
		return nil, errors.New("deployment file path must be clean and absolute")
	}
	initial, err := inspectStandardAuthoringContractPath(absolute)
	if err != nil || !initial.Mode().IsRegular() || initial.Size() < 0 || initial.Size() > standardAuthoringContractAssetReadLimit {
		return nil, errors.New("deployment file must be a bounded regular non-symlink file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, errors.New("open deployment file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(initial, opened) || opened.Size() != initial.Size() {
		return nil, errors.New("deployment file changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, standardAuthoringContractAssetReadLimit+1))
	if err != nil || len(contents) > standardAuthoringContractAssetReadLimit {
		return nil, errors.New("read deployment file")
	}
	final, err := file.Stat()
	pathInfo, pathErr := inspectStandardAuthoringContractPath(absolute)
	if err != nil || pathErr != nil || !final.Mode().IsRegular() || !pathInfo.Mode().IsRegular() || !os.SameFile(opened, final) || !os.SameFile(opened, pathInfo) || final.Size() != opened.Size() || pathInfo.Size() != opened.Size() {
		return nil, errors.New("deployment file changed while reading")
	}
	return append([]byte(nil), contents...), nil
}

func standardAuthoringDeploymentPathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
