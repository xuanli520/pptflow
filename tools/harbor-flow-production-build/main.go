// Command harbor-flow-production-build verifies the Standard authoring
// production deployment bundle and emits linker bindings for one Harbor Flow
// binary. It reads no environment values and accepts only catalog/lock paths
// plus the frozen source-manifest digest.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	standardAuthoringBuildModuleVariable                    = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildModule"
	standardAuthoringBuildVersionVariable                   = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildVersion"
	standardAuthoringBuildCommitVariable                    = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildCommit"
	standardAuthoringBuildDigestVariable                    = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildContentSHA256"
	standardAuthoringBuildCatalogReceiptFingerprintVariable = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildCatalogReceiptFingerprint"
	standardAuthoringBuildLockIDVariable                    = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildLockID"
	standardAuthoringBuildLockVersionVariable               = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildLockVersion"
	standardAuthoringBuildLockFingerprintVariable           = "github.com/purplevoid/harbor-factory/cmd.standardAuthoringProductionBuildLockFingerprint"
)

type productionBuildConfig struct {
	StandardAuthoringCatalog string
	StandardAuthoringLock    string
	SourceManifest           string
}

type bundleInput struct {
	name         string
	catalogLabel string
	lockLabel    string
	catalogPath  string
	lockPath     string
	variables    buildVariables
}

type buildVariables struct {
	module                    string
	version                   string
	commit                    string
	digest                    string
	catalogReceiptFingerprint string
	lockID                    string
	lockVersion               string
	lockFingerprint           string
}

type verifiedBundle struct {
	input    bundleInput
	verifier *stageprovider.DeploymentOperationCatalogLockResolver
}

type resolvedInputFile struct {
	label string
	path  string
	info  os.FileInfo
}

func main() {
	var config productionBuildConfig
	flag.StringVar(&config.StandardAuthoringCatalog, "standard-authoring-catalog", "", "Standard authoring deployment catalog")
	flag.StringVar(&config.StandardAuthoringLock, "standard-authoring-lock", "", "Standard authoring deployment lock")
	flag.StringVar(&config.SourceManifest, "source-manifest", "", "SHA-256 digest of the frozen source manifest")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("unexpected positional arguments")
	}
	flags, err := productionBuildLDFlags(config)
	if err != nil {
		fail(err.Error())
	}
	fmt.Print(flags)
}

func productionBuildLDFlags(config productionBuildConfig) (string, error) {
	config, err := normalizeProductionBuildConfig(config)
	if err != nil {
		return "", err
	}

	bundles := []bundleInput{
		{
			name: "Standard authoring", catalogLabel: "standard authoring catalog", lockLabel: "standard authoring lock",
			catalogPath: config.StandardAuthoringCatalog, lockPath: config.StandardAuthoringLock,
			variables: buildVariables{
				module: standardAuthoringBuildModuleVariable, version: standardAuthoringBuildVersionVariable,
				commit: standardAuthoringBuildCommitVariable, digest: standardAuthoringBuildDigestVariable,
				catalogReceiptFingerprint: standardAuthoringBuildCatalogReceiptFingerprintVariable,
				lockID:                    standardAuthoringBuildLockIDVariable, lockVersion: standardAuthoringBuildLockVersionVariable,
				lockFingerprint: standardAuthoringBuildLockFingerprintVariable,
			},
		},
	}

	verified := make([]verifiedBundle, 0, len(bundles))
	for _, input := range bundles {
		bundle, err := verifyBundle(input)
		if err != nil {
			return "", err
		}
		verified = append(verified, bundle)
	}
	if err := verifyDistinctBundleIdentities(verified); err != nil {
		return "", err
	}
	if err := verifySharedBuildIdentity(verified, workflowkit.Fingerprint(config.SourceManifest)); err != nil {
		return "", err
	}

	flags := make([]string, 0, len(verified)*8)
	for _, bundle := range verified {
		bundleFlags, err := linkerFlagsForBundle(bundle)
		if err != nil {
			return "", err
		}
		flags = append(flags, bundleFlags...)
	}
	return strings.Join(flags, " "), nil
}

func normalizeProductionBuildConfig(config productionBuildConfig) (productionBuildConfig, error) {
	if config.SourceManifest == "" || config.SourceManifest != strings.TrimSpace(config.SourceManifest) {
		return productionBuildConfig{}, fmt.Errorf("source-manifest is required and must be canonical")
	}
	if err := workflowkit.Fingerprint(config.SourceManifest).Validate(); err != nil {
		return productionBuildConfig{}, fmt.Errorf("source-manifest must be a SHA-256 fingerprint: %w", err)
	}

	inputs := []struct {
		label string
		value *string
	}{
		{label: "standard authoring catalog", value: &config.StandardAuthoringCatalog},
		{label: "standard authoring lock", value: &config.StandardAuthoringLock},
	}
	resolved := make([]resolvedInputFile, 0, len(inputs))
	for _, input := range inputs {
		file, err := resolveInputFile(input.label, *input.value)
		if err != nil {
			return productionBuildConfig{}, err
		}
		for _, existing := range resolved {
			if file.path == existing.path || os.SameFile(file.info, existing.info) {
				return productionBuildConfig{}, fmt.Errorf("duplicate deployment input: %s and %s resolve to the same file", existing.label, file.label)
			}
		}
		resolved = append(resolved, file)
		*input.value = file.path
	}
	return config, nil
}

func resolveInputFile(label, value string) (resolvedInputFile, error) {
	if value == "" || value != strings.TrimSpace(value) {
		return resolvedInputFile{}, fmt.Errorf("%s is required and must be canonical", label)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return resolvedInputFile{}, fmt.Errorf("resolve %s path: %w", label, err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return resolvedInputFile{}, fmt.Errorf("resolve %s path: %w", label, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return resolvedInputFile{}, fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return resolvedInputFile{}, fmt.Errorf("%s must be a regular file", label)
	}
	return resolvedInputFile{label: label, path: resolved, info: info}, nil
}

func verifyBundle(input bundleInput) (verifiedBundle, error) {
	catalogRaw, err := os.ReadFile(input.catalogPath)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("read %s: %w", input.catalogLabel, err)
	}
	catalogDocument, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("parse %s: %w", input.catalogLabel, err)
	}
	catalog, err := stageprovider.NewDeploymentOperationCatalogResolver(catalogDocument)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("resolve %s: %w", input.catalogLabel, err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(catalog.Template()) {
		return verifiedBundle{}, fmt.Errorf("%s template is %s@%s; want an installed Standard authoring template", input.catalogLabel, catalog.Template().ID, catalog.Template().Version)
	}

	lockRaw, err := os.ReadFile(input.lockPath)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("read %s: %w", input.lockLabel, err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(lockRaw)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("parse %s: %w", input.lockLabel, err)
	}
	if !workflowadapter.IsStandardAuthoringWorkflowTemplate(lock.CatalogReceipt.Template) {
		return verifiedBundle{}, fmt.Errorf("%s template is %s@%s; want an installed Standard authoring template", input.lockLabel, lock.CatalogReceipt.Template.ID, lock.CatalogReceipt.Template.Version)
	}
	verifier, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalog, lock)
	if err != nil {
		return verifiedBundle{}, fmt.Errorf("verify %s catalog and lock: %w", input.name, err)
	}
	return verifiedBundle{input: input, verifier: verifier}, nil
}

func verifyDistinctBundleIdentities(bundles []verifiedBundle) error {
	seenCatalogs := make(map[stageprovider.DeploymentOperationCatalogIdentity]string, len(bundles))
	seenLocks := make(map[stageprovider.DeploymentOperationCatalogLockIdentity]string, len(bundles))
	for _, bundle := range bundles {
		catalog := bundle.verifier.CatalogIdentity()
		if previous, duplicate := seenCatalogs[catalog]; duplicate {
			return fmt.Errorf("duplicate deployment catalog identity for %s and %s", previous, bundle.input.name)
		}
		seenCatalogs[catalog] = bundle.input.name

		lock := bundle.verifier.LockIdentity()
		if previous, duplicate := seenLocks[lock]; duplicate {
			return fmt.Errorf("duplicate deployment lock identity for %s and %s", previous, bundle.input.name)
		}
		seenLocks[lock] = bundle.input.name
	}
	return nil
}

func verifySharedBuildIdentity(bundles []verifiedBundle, sourceManifest workflowkit.Fingerprint) error {
	if len(bundles) != 1 {
		return fmt.Errorf("exactly one deployment bundle is required")
	}
	build := bundles[0].verifier.HarborFlowBuild()
	if build.ContentSHA256 != sourceManifest {
		return fmt.Errorf("%s lock source manifest does not match frozen source", bundles[0].input.name)
	}
	return nil
}

func linkerFlagsForBundle(bundle verifiedBundle) ([]string, error) {
	build := bundle.verifier.HarborFlowBuild()
	receiptFingerprint, err := bundle.verifier.CatalogReceipt().Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("fingerprint %s catalog receipt: %w", bundle.input.name, err)
	}
	lockIdentity := bundle.verifier.LockIdentity()
	values := []struct {
		label    string
		variable string
		value    string
	}{
		{label: "build module", variable: bundle.input.variables.module, value: build.Module},
		{label: "build version", variable: bundle.input.variables.version, value: build.Version},
		{label: "build commit", variable: bundle.input.variables.commit, value: build.Commit},
		{label: "build source manifest", variable: bundle.input.variables.digest, value: string(build.ContentSHA256)},
		{label: "catalog receipt fingerprint", variable: bundle.input.variables.catalogReceiptFingerprint, value: string(receiptFingerprint)},
		{label: "lock id", variable: bundle.input.variables.lockID, value: lockIdentity.LockID},
		{label: "lock version", variable: bundle.input.variables.lockVersion, value: lockIdentity.LockVersion},
		{label: "lock fingerprint", variable: bundle.input.variables.lockFingerprint, value: string(lockIdentity.Fingerprint)},
	}
	flags := make([]string, 0, len(values))
	for _, item := range values {
		if err := validateLinkerValue(bundle.input.name+" "+item.label, item.value); err != nil {
			return nil, err
		}
		flags = append(flags, "-X "+item.variable+"="+item.value)
	}
	return flags, nil
}

func validateLinkerValue(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty and cannot be injected into linker metadata", label)
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') {
			continue
		}
		switch character {
		case '.', '_', '-', '/', ':', '@', '+':
			continue
		default:
			return fmt.Errorf("%s contains unsafe linker character %q", label, character)
		}
	}
	return nil
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "harbor-flow-production-build:", message)
	os.Exit(1)
}
