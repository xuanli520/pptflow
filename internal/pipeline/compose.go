package pipeline

import (
	"path/filepath"
	"time"

	dockermgr "github.com/xuanli520/p2r_tui/internal/docker"
)

func readmes(repoPath string) []string {
	matches, _ := filepath.Glob(filepath.Join(repoPath, "README*"))
	return matches
}

func findCompose(repoPath string) string {
	return dockermgr.FindCompose(repoPath)
}

func readmeComposeCommand(repoPath string) []string {
	return dockermgr.ReadmeComposeCommand(repoPath)
}

func composeArgsWithProject(fields []string, projectName string) []string {
	return dockermgr.ComposeArgsWithProject(fields, projectName)
}

func composePSArgs(fields []string, projectName string) []string {
	return dockermgr.ComposePSArgs(fields, projectName)
}

func composePSQArgs(fields []string, projectName string) []string {
	return dockermgr.ComposePSQArgs(fields, projectName)
}

func composeServicesArgs(fields []string, projectName string) []string {
	return dockermgr.ComposeServicesArgs(fields, projectName)
}

func composeGlobals(fields []string, projectName string) []string {
	return dockermgr.ComposeGlobals(fields, projectName)
}

func composeCommandIndex(args []string) int {
	return dockermgr.ComposeCommandIndex(args)
}

func splitNonEmptyLines(value string) []string {
	return dockermgr.SplitNonEmptyLines(value)
}

func hasFlag(args []string, names ...string) bool {
	return dockermgr.HasFlag(args, names...)
}

func indexOf(args []string, value string) int {
	return dockermgr.IndexOf(args, value)
}

func parseComposePS(raw string) (map[string][]portMapping, []string) {
	return dockermgr.ParseComposePS(raw)
}

func probeMappings(mappings map[string][]portMapping, timeout time.Duration) []probeResult {
	return dockermgr.ProbeMappings(mappings, timeout)
}

func parseDockerPort(service, raw string) []portMapping {
	return dockermgr.ParseDockerPort(service, raw)
}
