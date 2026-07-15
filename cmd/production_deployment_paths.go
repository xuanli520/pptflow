package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
)

const (
	productionDeploymentsDirectory            = "deployments"
	standardAuthoringDeploymentDirectory      = "standard-authoring"
	codeEdgePhase1DeploymentDirectory         = "codeedge-phase1"
	codeEdgeEvaluatorChildDeploymentDirectory = "codeedge-evaluator-child"
	productionDeploymentCatalogFile           = "operation-catalog.v1.json"
	productionDeploymentLockFile              = "operation-catalog.lock.json"
	standardAuthoringContractManifestFile     = "contract-assets.v1.json"
)

// productionDeploymentPaths names the complete, immutable local materials
// required by one unified production binary.  All paths are resolved below
// the real executable directory; a caller cannot redirect one template to a
// user-owned catalog, lock, or contract asset tree.
type productionDeploymentPaths struct {
	StandardCatalog      string
	StandardLock         string
	StandardContractRoot string
	ParentCatalog        string
	ParentLock           string
	EvaluatorCatalog     string
	EvaluatorLock        string
}

func defaultProductionDeploymentPaths() (productionDeploymentPaths, error) {
	executable, err := os.Executable()
	if err != nil {
		return productionDeploymentPaths{}, fmt.Errorf("locate production executable: %w", err)
	}
	return productionDeploymentPathsBesideExecutable(executable)
}

func productionDeploymentPathsBesideExecutable(executable string) (productionDeploymentPaths, error) {
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return productionDeploymentPaths{}, fmt.Errorf("production executable path is required")
	}
	absoluteExecutable, err := filepath.Abs(executable)
	if err != nil {
		return productionDeploymentPaths{}, fmt.Errorf("resolve production executable: %w", err)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(absoluteExecutable)
	if err != nil {
		return productionDeploymentPaths{}, fmt.Errorf("resolve production executable symlinks: %w", err)
	}
	info, err := os.Lstat(resolvedExecutable)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return productionDeploymentPaths{}, fmt.Errorf("production executable must be a regular non-symlink file")
	}
	root := filepath.Dir(resolvedExecutable)
	deployments, err := requireCodeEdgeProductionManagedDirectory("deployment directory", filepath.Join(root, productionDeploymentsDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, fmt.Errorf("locate production deployment directory: %w", err)
	}
	standard, err := requireCodeEdgeProductionManagedDirectory("Standard authoring deployment directory", filepath.Join(deployments, standardAuthoringDeploymentDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	parent, err := requireCodeEdgeProductionManagedDirectory("CodeEdge Phase-1 deployment directory", filepath.Join(deployments, codeEdgePhase1DeploymentDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	evaluator, err := requireCodeEdgeProductionManagedDirectory("CodeEdge evaluator-child deployment directory", filepath.Join(deployments, codeEdgeEvaluatorChildDeploymentDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	paths := productionDeploymentPaths{StandardContractRoot: standard}
	for _, entry := range []struct {
		label       string
		directory   string
		catalogDest *string
		lockDest    *string
	}{
		{"Standard authoring", standard, &paths.StandardCatalog, &paths.StandardLock},
		{"CodeEdge Phase-1", parent, &paths.ParentCatalog, &paths.ParentLock},
		{"CodeEdge evaluator child", evaluator, &paths.EvaluatorCatalog, &paths.EvaluatorLock},
	} {
		catalog, catalogErr := requireCodeEdgeProductionFileWithin(entry.label+" catalog", filepath.Join(entry.directory, productionDeploymentCatalogFile), root)
		if catalogErr != nil {
			return productionDeploymentPaths{}, catalogErr
		}
		lock, lockErr := requireCodeEdgeProductionFileWithin(entry.label+" lock", filepath.Join(entry.directory, productionDeploymentLockFile), root)
		if lockErr != nil {
			return productionDeploymentPaths{}, lockErr
		}
		*entry.catalogDest, *entry.lockDest = catalog, lock
	}
	if _, err := requireCodeEdgeProductionFileWithin("Standard authoring contract asset manifest", filepath.Join(standard, standardAuthoringContractManifestFile), root); err != nil {
		return productionDeploymentPaths{}, err
	}
	if _, err := requireCodeEdgeProductionFileWithin("Standard authoring SSH known_hosts", filepath.Join(standard, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath)), root); err != nil {
		return productionDeploymentPaths{}, err
	}
	return paths, nil
}
