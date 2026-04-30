package pipeline

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func readmes(repoPath string) []string {
	matches, _ := filepath.Glob(filepath.Join(repoPath, "README*"))
	return matches
}

type portMapping struct {
	Service   string `json:"service"`
	URL       string `json:"url,omitempty"`
	Host      int    `json:"host"`
	Container int    `json:"container"`
	Protocol  string `json:"protocol,omitempty"`
}

type probeResult struct {
	Service string `json:"service"`
	URL     string `json:"url"`
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Error   string `json:"error,omitempty"`
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

func findCompose(repoPath string) string {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		path := filepath.Join(repoPath, name)
		if fileExists(path) {
			return path
		}
	}
	return ""
}

func readmeComposeCommand(repoPath string) []string {
	readmeFiles := readmes(repoPath)
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

func composeArgsWithProject(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	if !hasFlag(args, "-p", "--project-name") {
		args = append(args[:commandIndex], append([]string{"-p", projectName}, args[commandIndex:]...)...)
		commandIndex += 2
	}
	if !hasFlag(args, "-d", "--detach") {
		upIndex := indexOf(args, "up")
		if upIndex >= 0 {
			args = append(args, "-d")
		}
	}
	return args
}

func composePSArgs(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !hasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return append(globals, "ps", "--format", "json")
}

func composePSQArgs(fields []string, projectName string) []string {
	globals := composeGlobals(fields, projectName)
	return append(globals, "ps", "-q")
}

func composeServicesArgs(fields []string, projectName string) []string {
	globals := composeGlobals(fields, projectName)
	return append(globals, "config", "--services")
}

func composeGlobals(fields []string, projectName string) []string {
	args := append([]string{}, fields[1:]...)
	commandIndex := composeCommandIndex(args)
	if commandIndex < 0 {
		commandIndex = len(args)
	}
	globals := append([]string{}, args[:commandIndex]...)
	if !hasFlag(globals, "-p", "--project-name") {
		globals = append(globals, "-p", projectName)
	}
	return globals
}

func composeCommandIndex(args []string) int {
	for i, arg := range args {
		switch arg {
		case "up", "start", "run", "ps", "exec":
			return i
		}
	}
	return -1
}

func splitNonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func hasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}

func indexOf(args []string, value string) int {
	for i, arg := range args {
		if arg == value {
			return i
		}
	}
	return -1
}

func parseComposePS(raw string) (map[string][]portMapping, []string) {
	mappings := map[string][]portMapping{}
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
			mappings[name] = append(mappings[name], portMapping{
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

func probeMappings(mappings map[string][]portMapping, timeout time.Duration) []probeResult {
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	client := http.Client{Timeout: timeout}
	var results []probeResult
	for service, ports := range mappings {
		for _, mapping := range ports {
			if mapping.Host == 0 {
				continue
			}
			url := "http://127.0.0.1:" + strconv.Itoa(mapping.Host)
			response, err := client.Get(url)
			item := probeResult{Service: service, URL: url}
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

func parseDockerPort(service, raw string) []portMapping {
	var mappings []portMapping
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
		mappings = append(mappings, portMapping{
			Service:   service,
			URL:       strings.TrimSuffix(right[:lastColon], ":"),
			Host:      hostPort,
			Container: containerPort,
			Protocol:  protocol,
		})
	}
	return mappings
}
