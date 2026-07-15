// Command codeedge-production-build verifies a source-controlled CodeEdge
// production lock and emits the ldflags required to label the resulting
// harbor-factory binary. It deliberately has no model/provider configuration
// surface and never reads environment credentials.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/stageprovider"
)

const (
	buildModuleVariable                    = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildModule"
	buildVersionVariable                   = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildVersion"
	buildCommitVariable                    = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildCommit"
	buildDigestVariable                    = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildContentSHA256"
	buildCatalogReceiptFingerprintVariable = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildCatalogReceiptFingerprint"
	buildLockIDVariable                    = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildLockID"
	buildLockVersionVariable               = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildLockVersion"
	buildLockFingerprintVariable           = "github.com/purplevoid/harbor-factory/cmd.codeEdgeProductionBuildLockFingerprint"
)

func main() {
	var catalogPath, lockPath, sourceManifest string
	flag.StringVar(&catalogPath, "catalog", "", "source-controlled deployment catalog")
	flag.StringVar(&lockPath, "lock", "", "source-controlled deployment lock")
	flag.StringVar(&sourceManifest, "source-manifest", "", "sha256 of the frozen source manifest")
	flag.Parse()
	if flag.NArg() != 0 {
		fail("unexpected positional arguments")
	}
	flags, err := productionBuildLDFlags(catalogPath, lockPath, sourceManifest)
	if err != nil {
		fail(err.Error())
	}
	fmt.Print(flags)
}

func productionBuildLDFlags(catalogPath, lockPath, sourceManifest string) (string, error) {
	if strings.TrimSpace(catalogPath) == "" || strings.TrimSpace(lockPath) == "" || strings.TrimSpace(sourceManifest) == "" {
		return "", fmt.Errorf("catalog, lock, and source-manifest are required")
	}
	catalogRaw, err := os.ReadFile(catalogPath)
	if err != nil {
		return "", fmt.Errorf("read production catalog: %w", err)
	}
	catalog, err := stageprovider.ParseDeploymentOperationCatalogJSON(catalogRaw)
	if err != nil {
		return "", fmt.Errorf("parse production catalog: %w", err)
	}
	catalogResolver, err := stageprovider.NewDeploymentOperationCatalogResolver(catalog)
	if err != nil {
		return "", fmt.Errorf("resolve production catalog: %w", err)
	}
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return "", fmt.Errorf("read production lock: %w", err)
	}
	lock, err := stageprovider.ParseDeploymentOperationCatalogLockJSON(raw)
	if err != nil {
		return "", fmt.Errorf("parse production lock: %w", err)
	}
	if _, err := stageprovider.NewDeploymentOperationCatalogLockResolver(catalogResolver, lock); err != nil {
		return "", fmt.Errorf("verify production catalog and lock: %w", err)
	}
	build := lock.HarborFlowBuild
	if string(build.ContentSHA256) != sourceManifest {
		return "", fmt.Errorf("production lock source manifest does not match frozen source")
	}
	catalogReceiptFingerprint, err := lock.CatalogReceipt.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("fingerprint production catalog receipt: %w", err)
	}
	lockFingerprint, err := lock.Fingerprint()
	if err != nil {
		return "", fmt.Errorf("fingerprint production lock: %w", err)
	}
	for _, value := range []string{
		build.Module,
		build.Version,
		build.Commit,
		string(build.ContentSHA256),
		string(catalogReceiptFingerprint),
		lock.LockID,
		lock.LockVersion,
		string(lockFingerprint),
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, " \t\r\n'\"") {
			return "", fmt.Errorf("production build identity is not safe for linker injection")
		}
	}
	return strings.Join([]string{
		"-X " + buildModuleVariable + "=" + build.Module,
		"-X " + buildVersionVariable + "=" + build.Version,
		"-X " + buildCommitVariable + "=" + build.Commit,
		"-X " + buildDigestVariable + "=" + string(build.ContentSHA256),
		"-X " + buildCatalogReceiptFingerprintVariable + "=" + string(catalogReceiptFingerprint),
		"-X " + buildLockIDVariable + "=" + lock.LockID,
		"-X " + buildLockVersionVariable + "=" + lock.LockVersion,
		"-X " + buildLockFingerprintVariable + "=" + string(lockFingerprint),
	}, " "), nil
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "codeedge-production-build:", message)
	os.Exit(1)
}
