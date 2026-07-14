package agent_test

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
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
