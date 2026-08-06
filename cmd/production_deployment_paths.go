package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
)

const (
	productionDeploymentsDirectory        = "deployments"
	standardAuthoringDeploymentDirectory  = "standard-authoring"
	productionDeploymentCatalogFile       = "operation-catalog.v1.json"
	productionDeploymentLockFile          = "operation-catalog.lock.json"
	standardAuthoringContractManifestFile = "contract-assets.v1.json"
)

// productionDeploymentPaths names the complete, immutable local materials
// required by one unified production binary.  All paths are resolved below
// the real executable directory; a caller cannot redirect one template to a
// user-owned catalog, lock, or contract asset tree.
type productionDeploymentPaths struct {
	StandardCatalog      string
	StandardLock         string
	StandardContractRoot string
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
	deployments, err := requireManagedProductionDirectory("deployment directory", filepath.Join(root, productionDeploymentsDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, fmt.Errorf("locate production deployment directory: %w", err)
	}
	standard, err := requireManagedProductionDirectory("Standard authoring deployment directory", filepath.Join(deployments, standardAuthoringDeploymentDirectory), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	catalog, err := requireManagedProductionFileWithin("Standard authoring catalog", filepath.Join(standard, productionDeploymentCatalogFile), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	lock, err := requireManagedProductionFileWithin("Standard authoring lock", filepath.Join(standard, productionDeploymentLockFile), root)
	if err != nil {
		return productionDeploymentPaths{}, err
	}
	if _, err := requireManagedProductionFileWithin("Standard authoring contract asset manifest", filepath.Join(standard, standardAuthoringContractManifestFile), root); err != nil {
		return productionDeploymentPaths{}, err
	}
	if _, err := requireManagedProductionFileWithin("Standard authoring SSH known_hosts", filepath.Join(standard, filepath.FromSlash(stageprovider.StandardAuthoringSSHKnownHostsRelativePath)), root); err != nil {
		return productionDeploymentPaths{}, err
	}
	return productionDeploymentPaths{StandardCatalog: catalog, StandardLock: lock, StandardContractRoot: standard}, nil
}

// requireManagedProductionDirectory accepts one deployment directory only
// when it is a real directory below the resolved executable directory. Both
// deployment path components are checked separately so a symlink cannot
// redirect an otherwise regular catalog or lock outside the local package.
func requireManagedProductionDirectory(label, path, executableDirectory string) (string, error) {
	path = strings.TrimSpace(path)
	executableDirectory = strings.TrimSpace(executableDirectory)
	if path == "" || executableDirectory == "" {
		return "", fmt.Errorf("production %s path is required", label)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve production %s path: %w", label, err)
	}
	if !managedProductionPathWithin(executableDirectory, absolute) {
		return "", fmt.Errorf("production %s escapes the resolved executable directory", label)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect production %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("production %s must be a non-symlink directory", label)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve production %s: %w", label, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(absolute) || !managedProductionPathWithin(executableDirectory, resolved) {
		return "", fmt.Errorf("production %s escapes the resolved executable directory", label)
	}
	return filepath.Clean(absolute), nil
}

// requireManagedProductionFileWithin retains the regular-file/no-final-
// symlink rule and additionally proves that resolving every path component
// still names a file inside the resolved executable directory.
func requireManagedProductionFileWithin(label, path, executableDirectory string) (string, error) {
	file, err := requireManagedProductionFile(label, path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(file)
	if err != nil {
		return "", fmt.Errorf("resolve production %s: %w", label, err)
	}
	if filepath.Clean(resolved) != filepath.Clean(file) || !managedProductionPathWithin(executableDirectory, resolved) {
		return "", fmt.Errorf("production %s escapes the resolved executable directory", label)
	}
	return file, nil
}

func managedProductionPathWithin(root, path string) bool {
	root, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return false
	}
	path, err = filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	return true
}

func requireManagedProductionFile(label, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("production %s path is required", label)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve production %s path: %w", label, err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect production %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("production %s must be a regular non-symlink file", label)
	}
	return filepath.Clean(absolute), nil
}
