package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func taskInputConfigFixture() taskInputConfig {
	return taskInputConfig{
		Format:        taskInputConfigFormat,
		Version:       taskInputConfigVersion,
		RepositoryURL: "https://github.com/rust-lang/rustlings.git",
		CommitSHA:     "734461f2fb8c7bb8403f4a9bd1fc7f983d32860b",
		BaseImage:     taskBoardTestBaseImage,
		Slug:          "rustlings-empty-input-bugfix",
		Title:         "Fix empty input handling",
		TaskType:      "bugfix",
		Application:   "rustlings",
		CodeLanguage:  "rust",
		Is0To1:        false,
		Objective:     "Handle empty input without a panic and add regression coverage.",
		Reason:        "Create a bounded authoring task from the reviewed configuration.",
	}
}

func writeTaskInputConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func taskInputConfigJSON(config taskInputConfig) string {
	return fmt.Sprintf(`{
  "format": %q,
  "version": %q,
  "repository_url": %q,
  "commit_sha": %q,
  "base_image": %q,
  "slug": %q,
  "title": %q,
  "task_type": %q,
  "application": %q,
  "code_language": %q,
  "is_0_to_1": %t,
  "objective": %q,
  "reason": %q
}`,
		config.Format, config.Version, config.RepositoryURL, config.CommitSHA, config.BaseImage, config.Slug, config.Title,
		config.TaskType, config.Application, config.CodeLanguage, config.Is0To1, config.Objective, config.Reason)
}

func TestReadTaskInputConfigFileAcceptsStrictValidConfig(t *testing.T) {
	want := taskInputConfigFixture()
	got, err := readTaskInputConfigFile(writeTaskInputConfig(t, taskInputConfigJSON(want)))
	if err != nil || got != want {
		t.Fatalf("read config = %+v, %v; want %+v", got, err, want)
	}
}

func TestYewFrontendHardBugfixConfigExampleLoads(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "examples", "yew-frontend-hard-bugfix.json")
	config, err := readTaskInputConfigFile(path)
	if err != nil {
		t.Fatalf("read config example: %v", err)
	}
	if config.RepositoryURL != "https://github.com/yewstack/yew.git" || config.TaskType != "bugfix" || config.Application != "frontend" || config.CodeLanguage != "rust" || config.Is0To1 {
		t.Fatalf("config example has unexpected task facts: %+v", config)
	}
}

func TestReadTaskInputConfigFileRejectsInvalidInputs(t *testing.T) {
	valid := taskInputConfigJSON(taskInputConfigFixture())
	for _, test := range []struct {
		name     string
		contents string
	}{
		{"unknown field", strings.Replace(valid, "\n}", ",\n  \"unexpected\": true\n}", 1)},
		{"duplicate key", strings.Replace(valid, "\n}", ",\n  \"slug\": \"duplicate\"\n}", 1)},
		{"wrong format", strings.Replace(valid, taskInputConfigFormat, "harbor.task-input.v0", 1)},
		{"wrong commit", strings.Replace(valid, taskInputConfigFixture().CommitSHA, "ABC", 1)},
		{"invalid application token", strings.Replace(valid, `"application": "rustlings"`, `"application": "browser_wasm"`, 1)},
		{"trailing document", valid + "\n{}"},
		{"invalid JSON", "{invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readTaskInputConfigFile(writeTaskInputConfig(t, test.contents)); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
}

func TestTaskInputConfigLoadPreservesExistingValuesOnFailureAndAllowsEditingBeforeSubmit(t *testing.T) {
	input := NewTaskInputModel()
	input.Show()
	input.repoInput.SetValue("https://example.invalid/keep.git")
	input.BeginConfigLoad()
	input.SetConfigLoadError(fmt.Errorf("malformed JSON"))
	if input.repoInput.Value() != "https://example.invalid/keep.git" || input.mode != taskInputLoadConfig || !strings.Contains(input.validationErr, "malformed JSON") {
		t.Fatalf("failed config load altered form: repo=%q mode=%d error=%q", input.repoInput.Value(), input.mode, input.validationErr)
	}

	config := taskInputConfigFixture()
	input.ApplyConfig(config)
	input.titleInput.SetValue("Corrected empty input handling")
	command, handled := input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatal("loaded form did not submit")
	}
	message, ok := command().(TaskSubmitMsg)
	if !ok || message.RepoURL != config.RepositoryURL || message.Title != "Corrected empty input handling" || message.Reason != config.Reason || message.TaskType != config.TaskType {
		t.Fatalf("loaded submit message = %#v", message)
	}
}

func TestAppLoadsFileConfigBeforeStartingAuthoring(t *testing.T) {
	stub := &taskBoardGatewayStub{snapshot: taskBoardTestSnapshot(true)}
	model := loadedTaskBoardModel(t, stub)
	path := writeTaskInputConfig(t, taskInputConfigJSON(taskInputConfigFixture()))
	updated, _ := model.handleKey(keyRune('n'), nil)
	model = updated.(appModel)
	if !model.input.Visible() || model.input.mode != taskInputLoadConfig {
		t.Fatalf("new task did not open config loading state: visible=%t mode=%d", model.input.Visible(), model.input.mode)
	}
	model.input.configPathInput.SetValue(path)
	command, handled := model.input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled {
		t.Fatal("config path key was not handled")
	}
	if command == nil {
		t.Fatal("config path did not emit a request")
	}
	request, ok := command().(TaskConfigLoadRequestMsg)
	if !ok || request.Path != path || stub.keys != 0 || len(stub.startRequests) != 0 {
		t.Fatalf("config load request = %#v, keys=%d starts=%d", request, stub.keys, len(stub.startRequests))
	}
	updatedModel, loadCommand := model.Update(request)
	model = updatedModel.(appModel)
	loaded, ok := loadCommand().(TaskConfigLoadedMsg)
	if !ok || loaded.Err != nil {
		t.Fatalf("config load result = %#v", loaded)
	}
	updatedModel, _ = model.Update(loaded)
	model = updatedModel.(appModel)
	if model.input.mode != taskInputEdit || model.input.repoInput.Value() != taskInputConfigFixture().RepositoryURL || stub.keys != 0 || len(stub.startRequests) != 0 {
		t.Fatalf("loaded form state = mode:%d repo:%q keys:%d starts:%d", model.input.mode, model.input.repoInput.Value(), stub.keys, len(stub.startRequests))
	}
	model.input.titleInput.SetValue("Corrected title")
	command, handled = model.input.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !handled || command == nil {
		t.Fatal("loaded form could not submit")
	}
	updatedModel, startCommand := model.Update(command())
	model = updatedModel.(appModel)
	_ = model
	mutation, ok := startCommand().(taskBoardMutationMsg)
	if !ok || mutation.err != nil || len(stub.startRequests) != 1 || stub.startRequests[0].Title != "Corrected title" {
		t.Fatalf("start from loaded config = %#v, requests=%+v", mutation, stub.startRequests)
	}
}
