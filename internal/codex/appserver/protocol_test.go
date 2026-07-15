package appserver

import "testing"

func TestAppServerTurnStartParamsUsesMultimodalInput(t *testing.T) {
	params := appServerTurnStartParams(Request{
		ProjectPath:   `H:\project\harbor_factory\workspace`,
		Prompt:        "analyze this repository evidence",
		SandboxPolicy: "readWrite",
		WorkspaceRoots: []string{
			`H:\project\harbor_factory\workspace`,
			`H:\project\harbor_factory\artifacts`,
		},
		Input: []InputPart{
			{Type: "localImage", Path: `H:\project\harbor_factory\artifacts\terminal.png`, Detail: "high"},
			{Type: "image", URL: "https://example.com/result.png", Detail: "original"},
		},
	}, "thread-1")

	input, ok := params["input"].([]map[string]any)
	if !ok {
		t.Fatalf("input type = %T", params["input"])
	}
	if len(input) != 3 {
		t.Fatalf("input length = %d", len(input))
	}
	if input[0]["type"] != "text" || input[0]["text"] != "analyze this repository evidence" {
		t.Fatalf("text input = %+v", input[0])
	}
	if input[1]["type"] != "localImage" || input[1]["path"] == "" || input[1]["detail"] != "high" {
		t.Fatalf("local image input = %+v", input[1])
	}
	if input[2]["type"] != "image" || input[2]["url"] == "" || input[2]["detail"] != "original" {
		t.Fatalf("image input = %+v", input[2])
	}

	policy, ok := params["sandboxPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("sandboxPolicy type = %T", params["sandboxPolicy"])
	}
	if policy["type"] != "workspaceWrite" {
		t.Fatalf("sandbox type = %v", policy["type"])
	}
	roots, ok := policy["writableRoots"].([]string)
	if !ok || len(roots) != 2 {
		t.Fatalf("writable roots = %#v", policy["writableRoots"])
	}
	runtimeRoots, ok := params["runtimeWorkspaceRoots"].([]string)
	if !ok || len(runtimeRoots) != 2 {
		t.Fatalf("runtime roots = %#v", params["runtimeWorkspaceRoots"])
	}
}

func TestAppServerClientInfoIsGenericAndConfigurable(t *testing.T) {
	defaults := appServerClientInfo(Request{})
	if defaults["name"] != "agent-runtime" || defaults["version"] != "1" {
		t.Fatalf("generic defaults = %#v", defaults)
	}
	configured := appServerClientInfo(Request{ClientName: "controlled-standard-flow", ClientVersion: "v2"})
	if configured["name"] != "controlled-standard-flow" || configured["version"] != "v2" {
		t.Fatalf("configured client info = %#v", configured)
	}
}
