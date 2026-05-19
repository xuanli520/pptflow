package docker

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func Readmes(repoPath string) []string {
	matches, _ := filepath.Glob(filepath.Join(repoPath, "README*"))
	return matches
}

type composePSService struct {
	Service    string `json:"Service"`
	Name       string `json:"Name"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

func FindCompose(repoPath string) string {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		path := filepath.Join(repoPath, name)
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func ReadmeComposeCommand(repoPath string) []string {
	readmeFiles := Readmes(repoPath)
	for _, readme := range readmeFiles {
		content, err := os.ReadFile(readme)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "$")
			line = strings.TrimPrefix(line, ">")
			line = strings.TrimSpace(strings.Trim(line, "`"))
			lower := strings.ToLower(line)
			if !(strings.Contains(lower, "docker compose") || strings.Contains(lower, "docker-compose")) || !strings.Contains(lower, " up") {
				continue
			}
			fields := strings.Fields(line)
			clean := make([]string, 0, len(fields))
			for _, field := range fields {
				if field == "&&" || field == ";" || field == "|" {
					break
				}
				clean = append(clean, field)
			}
			if len(clean) >= 3 && clean[0] == "docker" && clean[1] == "compose" {
				return clean
			}
			if len(clean) >= 2 && clean[0] == "docker-compose" {
				return append([]string{"docker", "compose"}, clean[1:]...)
			}
		}
	}
	return nil
}

func ComposeArgsWithProject(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := ComposeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	if !HasFlag(args, "-p", "--project-name") {
		args = append(args[:commandIndex], append([]string{"-p", projectName}, args[commandIndex:]...)...)
		commandIndex += 2
	}
	if !HasFlag(args, "-d", "--detach") {
		upIndex := IndexOf(args, "up")
		if upIndex >= 0 {
			args = append(args, "-d")
		}
	}
	return args
}

func ComposePSArgs(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := ComposeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !HasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return append(globals, "ps", "--format", "json")
}

func ComposePSQArgs(fields []string, projectName string) []string {
	globals := ComposeGlobals(fields, projectName)
	return append(globals, "ps", "-q")
}

func ComposeServicesArgs(fields []string, projectName string) []string {
	globals := ComposeGlobals(fields, projectName)
	return append(globals, "config", "--services")
}

func ComposeGlobals(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := ComposeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !HasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return globals
}

func ComposeCommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "up", "start", "run", "ps", "exec":
			return i
		}
	}
	return -1
}

func SplitNonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func HasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func IndexOf(args []string, value string) int {
	for i, arg := range args {
		if arg == value {
			return i
		}
	}
	return -1
}

func ComposeFileArgs(files []string) []string {
	args := []string{}
	for _, file := range files {
		if strings.TrimSpace(file) == "" {
			continue
		}
		args = append(args, "-f", file)
	}
	return args
}

func ComposeCommandArgs(files []string, projectName string, tail ...string) []string {
	return ComposeCommandArgsWithProjectDir(files, "", projectName, tail...)
}

func ComposeCommandArgsWithProjectDir(files []string, projectDir, projectName string, tail ...string) []string {
	args := []string{"compose"}
	if strings.TrimSpace(projectDir) != "" {
		args = append(args, "--project-directory", projectDir)
	}
	args = append(args, ComposeFileArgs(files)...)
	args = append(args, "-p", projectName)
	args = append(args, tail...)
	return args
}

func ParseComposePS(raw string) (map[string][]PortMapping, []string) {
	mappings := map[string][]PortMapping{}
	serviceSet := map[string]bool{}
	var services []composePSService
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return mappings, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		_ = json.Unmarshal([]byte(trimmed), &services)
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var service composePSService
			if json.Unmarshal([]byte(line), &service) == nil {
				services = append(services, service)
			}
		}
	}
	var serviceNames []string
	for _, service := range services {
		name := service.Service
		if name == "" {
			name = service.Name
		}
		if name == "" {
			continue
		}
		if !serviceSet[name] {
			serviceNames = append(serviceNames, name)
			serviceSet[name] = true
		}
		for _, publisher := range service.Publishers {
			if publisher.PublishedPort == 0 {
				continue
			}
			mappings[name] = append(mappings[name], PortMapping{
				Service:   name,
				URL:       publisher.URL,
				Host:      publisher.PublishedPort,
				Container: publisher.TargetPort,
				Protocol:  publisher.Protocol,
			})
		}
	}
	return mappings, serviceNames
}

func ProbeMappings(mappings map[string][]PortMapping, timeout time.Duration) []ProbeResult {
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	client := http.Client{Timeout: timeout}
	var results []ProbeResult
	for service, ports := range mappings {
		for _, mapping := range ports {
			if mapping.Host == 0 {
				continue
			}
			url := "http://127.0.0.1:" + strconv.Itoa(mapping.Host)
			response, err := client.Get(url)
			item := ProbeResult{Service: service, URL: url}
			if err != nil {
				item.Error = err.Error()
			} else {
				item.OK = response.StatusCode >= 200 && response.StatusCode < 500
				item.Status = response.StatusCode
				_ = response.Body.Close()
			}
			results = append(results, item)
		}
	}
	return results
}

func ParseDockerPort(service, raw string) []PortMapping {
	var mappings []PortMapping
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "->") {
			continue
		}
		parts := strings.SplitN(line, "->", 2)
		left := strings.TrimSpace(parts[0])
		right := strings.TrimSpace(parts[1])
		containerPort := 0
		protocol := ""
		if portParts := strings.SplitN(left, "/", 2); len(portParts) == 2 {
			containerPort, _ = strconv.Atoi(portParts[0])
			protocol = portParts[1]
		}
		lastColon := strings.LastIndex(right, ":")
		if lastColon < 0 {
			continue
		}
		hostText := right[lastColon+1:]
		hostPort, _ := strconv.Atoi(hostText)
		if hostPort == 0 {
			continue
		}
		mappings = append(mappings, PortMapping{
			Service:   service,
			URL:       strings.TrimSuffix(right[:lastColon], ":"),
			Host:      hostPort,
			Container: containerPort,
			Protocol:  protocol,
		})
	}
	return mappings
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func pathWithinRoot(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
