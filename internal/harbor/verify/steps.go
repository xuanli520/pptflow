package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
)

type EnvironmentMode string

const (
	EnvironmentDockerfile EnvironmentMode = "dockerfile"
	EnvironmentCompose    EnvironmentMode = "compose"
)

type Environment struct {
	Mode           EnvironmentMode
	TaskDir        string
	Dockerfile     string
	ComposeFile    string
	ImageTag       string
	ComposeProject string
}

type CommandSpec struct {
	Name    string
	Dir     string
	Command string
	Args    []string
}

func ResolveEnvironment(taskDir, imageTag, composeProject string) (Environment, error) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return Environment{}, fmt.Errorf("task directory is required")
	}
	environment := Environment{
		TaskDir:        taskDir,
		ImageTag:       strings.TrimSpace(imageTag),
		ComposeProject: strings.TrimSpace(composeProject),
	}
	dockerfile := filepath.Join(taskDir, "environment", "Dockerfile")
	if info, err := os.Stat(dockerfile); err == nil && info.Mode().IsRegular() {
		if environment.ImageTag == "" {
			return Environment{}, fmt.Errorf("image tag is required for Dockerfile verification")
		}
		environment.Mode = EnvironmentDockerfile
		environment.Dockerfile = dockerfile
		return environment, nil
	}
	compose := filepath.Join(taskDir, "environment", "docker-compose.yaml")
	if info, err := os.Stat(compose); err == nil && info.Mode().IsRegular() {
		if environment.ComposeProject == "" {
			return Environment{}, fmt.Errorf("compose project is required for compose verification")
		}
		environment.Mode = EnvironmentCompose
		environment.ComposeFile = compose
		return environment, nil
	}
	return Environment{}, fmt.Errorf("environment must contain Dockerfile or docker-compose.yaml")
}

func DockerBuildCommand(environment Environment) CommandSpec {
	if environment.Mode == EnvironmentCompose {
		return CommandSpec{
			Name: "docker_build", Dir: environment.TaskDir, Command: "docker",
			Args: []string{"compose", "-p", environment.ComposeProject, "-f", environment.ComposeFile, "--project-directory", environment.TaskDir, "build", "main"},
		}
	}
	return CommandSpec{
		Name: "docker_build", Command: "docker",
		Args: []string{"build", "-t", environment.ImageTag, "-f", environment.Dockerfile, filepath.Join(environment.TaskDir, "environment")},
	}
}

func InitialVerifyCommand(environment Environment) CommandSpec {
	testsDir := filepath.Join(environment.TaskDir, "tests")
	if environment.Mode == EnvironmentCompose {
		return CommandSpec{
			Name: "initial_verify", Dir: environment.TaskDir, Command: "docker",
			Args: []string{"compose", "-p", environment.ComposeProject, "-f", environment.ComposeFile, "--project-directory", environment.TaskDir, "run", "--rm", "-v", testsDir + ":/tests:ro", "main", "/bin/sh", "-c", "/tests/test.sh"},
		}
	}
	return CommandSpec{
		Name: "initial_verify", Command: "docker",
		Args: []string{"run", "--rm", "-v", testsDir + ":/tests:ro", environment.ImageTag, "/bin/sh", "-c", "/tests/test.sh"},
	}
}

func OracleVerifyCommand(environment Environment) CommandSpec {
	solutionDir := filepath.Join(environment.TaskDir, "solution")
	testsDir := filepath.Join(environment.TaskDir, "tests")
	if environment.Mode == EnvironmentCompose {
		return CommandSpec{
			Name: "oracle_verify", Dir: environment.TaskDir, Command: "docker",
			Args: []string{"compose", "-p", environment.ComposeProject, "-f", environment.ComposeFile, "--project-directory", environment.TaskDir, "run", "--rm", "-v", solutionDir + ":/solution:ro", "-v", testsDir + ":/tests:ro", "main", "/bin/sh", "-c", "/solution/solve.sh && /tests/test.sh"},
		}
	}
	return CommandSpec{
		Name: "oracle_verify", Command: "docker",
		Args: []string{"run", "--rm", "-v", solutionDir + ":/solution:ro", "-v", testsDir + ":/tests:ro", environment.ImageTag, "/bin/sh", "-c", "/solution/solve.sh && /tests/test.sh"},
	}
}

func InitialVerificationExposesIssue(run domain.CommandRun) bool {
	return initialVerificationExposesIssue(run)
}
