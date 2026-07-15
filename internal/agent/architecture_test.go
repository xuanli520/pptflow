package agent_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/purplevoid/harbor-factory"

func TestAgentPortAndV2ConsumersDoNotImportLegacyWorkflow(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	cases := []struct {
		relativePath string
		wantAgent    bool
	}{
		{relativePath: "internal/agent/types.go"},
		{relativePath: "internal/agent/turn.go"},
		{relativePath: "internal/runtime/codexruntime/runtime.go", wantAgent: true},
		{relativePath: "internal/app/change_provider.go", wantAgent: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.relativePath, func(t *testing.T) {
			imports := sourceImports(t, filepath.Join(root, testCase.relativePath))
			if imports[modulePath+"/internal/workflow"] {
				t.Fatalf("%s imports legacy workflow", testCase.relativePath)
			}
			if testCase.wantAgent && !imports[modulePath+"/internal/agent"] {
				t.Fatalf("%s does not depend on the Agent port", testCase.relativePath)
			}
		})
	}
}

// The Codex App Server runtime is a retained general Agent Runtime, not part
// of the retired Harbor execution graph. Keep its source present and prevent
// the reusable process/session layer from acquiring Harbor workflow, store,
// CLI, or TUI dependencies.
func TestCodexAppServerRuntimeRemainsPresentAndGeneric(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	runtimeRoots := []string{
		"internal/agent",
		"internal/codex",
		"internal/executor",
		"internal/runtime/codexruntime",
	}
	required := []string{
		"internal/agent/types.go",
		"internal/agent/turn.go",
		"internal/codex/cli.go",
		"internal/codex/sandbox.go",
		"internal/codex/appserver/session.go",
		"internal/codex/appserver/stream.go",
		"internal/executor/cmd.go",
		"internal/executor/process_unix.go",
		"internal/executor/process_windows.go",
		"internal/runtime/codexruntime/runtime.go",
	}
	for _, relativePath := range required {
		info, err := os.Stat(filepath.Join(root, relativePath))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("retained generic Agent Runtime source %s is unavailable: %v", relativePath, err)
		}
	}
	capabilities := map[string][]string{
		"internal/agent/types.go": {
			"type StreamingConversation interface",
			"type SteerableConversation interface",
		},
		"internal/codex/appserver/session.go": {
			"func (s *appServerSession) Turn(",
			"func (s *appServerSession) SendGuidance(",
		},
		"internal/codex/appserver/stream.go": {
			"func (s *appServerSession) readStdout(",
			"func (s *appServerSession) readStderr(",
		},
		"internal/executor/cmd.go": {
			"func (Runner) RunStreamingWithOutput(",
			"func ConfigureCommand(",
		},
		"internal/executor/process_unix.go": {
			"Setpgid: true",
			"syscall.SIGTERM",
			"syscall.SIGKILL",
		},
	}
	for relativePath, snippets := range capabilities {
		contents, err := os.ReadFile(filepath.Join(root, relativePath))
		if err != nil {
			t.Fatalf("read retained capability source %s: %v", relativePath, err)
		}
		for _, snippet := range snippets {
			if !strings.Contains(string(contents), snippet) {
				t.Fatalf("retained generic Agent Runtime source %s no longer provides %q", relativePath, snippet)
			}
		}
	}
	forbidden := []string{
		modulePath + "/internal/app",
		modulePath + "/internal/harbor",
		modulePath + "/internal/tui",
		modulePath + "/internal/workflowruntime",
		modulePath + "/internal/workflow",
		modulePath + "/pkg/workflowkit",
	}
	var violations []string
	for _, relativeRoot := range runtimeRoots {
		err := filepath.WalkDir(filepath.Join(root, relativeRoot), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			imports := sourceImports(t, path)
			relativePath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			for imported := range imports {
				for _, banned := range forbidden {
					if imported == banned || strings.HasPrefix(imported, banned+"/") {
						violations = append(violations, filepath.ToSlash(relativePath)+" imports product execution dependency "+imported)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk retained generic Agent Runtime source %s: %v", relativeRoot, err)
		}
	}
	if len(violations) != 0 {
		t.Fatalf("Codex App Server Agent Runtime leaked Harbor execution dependencies:\n%s", strings.Join(violations, "\n"))
	}
}

// The public-internal Agent port is deliberately smaller than a workflow or
// deployment contract.  A provider-specific caller may adapt it to such a
// contract outside this package, but no Harbor/Catalog vocabulary or product
// dependency may be added here.
func TestAgentPortHasNoProductDependenciesOrSemantics(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	var violations []string
	err := filepath.WalkDir(filepath.Join(root, "internal", "agent"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for imported := range sourceImports(t, path) {
			if strings.HasPrefix(imported, modulePath+"/") {
				violations = append(violations, filepath.ToSlash(relativePath)+" imports product package "+imported)
			}
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lower := strings.ToLower(string(contents))
		for _, forbidden := range []string{"harbor", "catalog", "codeedge", "deployment"} {
			if strings.Contains(lower, forbidden) {
				violations = append(violations, filepath.ToSlash(relativePath)+" contains product-specific term "+forbidden)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk Agent port: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("Agent port is no longer generic:\n%s", strings.Join(violations, "\n"))
	}
}

func TestControlledCodexRuntimeCannotRegressToAmbientDiscovery(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	path := filepath.Join(root, "internal", "runtime", "codexruntime", "runtime.go")
	imports := sourceImports(t, path)
	allowed := map[string]bool{
		modulePath + "/internal/agent":           true,
		modulePath + "/internal/codex":           true,
		modulePath + "/internal/codex/appserver": true,
		modulePath + "/internal/executor":        true,
	}
	for imported := range imports {
		if strings.HasPrefix(imported, modulePath+"/") && !allowed[imported] {
			t.Fatalf("controlled Codex runtime imports non-runtime package %s", imported)
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"codex.DetectCLI(",
		"codex.InspectCLI(",
		"prepareAutomationCodexHome",
		"copyCodexHomeFile",
		`os.Getenv("CODEX_HOME")`,
	} {
		if strings.Contains(string(contents), forbidden) {
			t.Fatalf("controlled Codex runtime reintroduced ambient behavior %q", forbidden)
		}
	}
}

func TestCodexRuntimeImplementsOptionalGenericConversationCapabilities(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test source path")
	}
	path := filepath.Join(filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..")), "internal", "runtime", "codexruntime", "runtime.go")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, assertion := range []string{
		"_ agent.Conversation          = (*conversation)(nil)",
		"_ agent.StreamingConversation = (*conversation)(nil)",
		"_ agent.SteerableConversation = (*conversation)(nil)",
	} {
		if !strings.Contains(string(contents), assertion) {
			t.Fatalf("Codex runtime is missing generic capability assertion %q", assertion)
		}
	}
}

func sourceImports(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse imports for %s: %v", path, err)
	}
	imports := make(map[string]bool, len(file.Imports))
	for _, specification := range file.Imports {
		value, err := strconv.Unquote(specification.Path.Value)
		if err != nil {
			t.Fatalf("unquote import %q in %s: %v", specification.Path.Value, path, err)
		}
		imports[value] = true
	}
	return imports
}
