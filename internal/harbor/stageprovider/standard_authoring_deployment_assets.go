package stageprovider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
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
	if !catalog.Template().Equal(workflowadapter.StandardAuthoringTemplateReference()) {
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
	if !verifier.CatalogReceipt().Template.Equal(workflowadapter.StandardAuthoringTemplateReference()) {
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
		if contract.Prompt != entry.Prompt || contract.Schema != entry.Schema {
			return nil, fmt.Errorf("%w: generated Standard authoring lock contract differs from manifest for stage %q", ErrDeploymentOperationCatalogLockDrift, record.Stage.Key)
		}
		if _, err := readStandardAuthoringContractAsset(context.Background(), contractRoot, contract.Prompt, record.PromptContentFingerprint); err != nil {
			return nil, fmt.Errorf("verify Standard authoring prompt asset for stage %q: %w", record.Stage.Key, err)
		}
		if _, err := readStandardAuthoringContractAsset(context.Background(), contractRoot, contract.Schema, record.SchemaContentFingerprint); err != nil {
			return nil, fmt.Errorf("verify Standard authoring schema asset for stage %q: %w", record.Stage.Key, err)
		}
	}
	return &StandardAuthoringDeploymentAssetBundle{
		Catalog: catalog, Lock: verifier.Lock(), Verifier: verifier, Manifest: manifest.Clone(), ContractRoot: contractRoot,
	}, nil
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
