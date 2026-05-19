package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

type GCRunRequest struct {
	ScanPath     string
	Config       config.DockerConfig
	Exec         executor.CommandRunner
	DryRun       bool
	Yes          bool
	AllowGlobal  bool
	Trigger      string
	ArtifactRoot string
	SkipReason   string
}

type GCSummary struct {
	OK                   bool       `json:"ok"`
	DryRun               bool       `json:"dry_run"`
	Trigger              string     `json:"trigger"`
	StartedAt            string     `json:"started_at"`
	FinishedAt           string     `json:"finished_at"`
	DurationMS           int64      `json:"duration_ms"`
	LockPath             string     `json:"lock_path,omitempty"`
	Skipped              bool       `json:"skipped"`
	SkipReason           string     `json:"skip_reason,omitempty"`
	P2ROnly              bool       `json:"p2r_only"`
	ManagedLabel         string     `json:"managed_label"`
	ComposeProjectPrefix string     `json:"compose_project_prefix"`
	Actions              []GCAction `json:"actions"`
	Commands             []string   `json:"commands"`
	Error                string     `json:"error,omitempty"`
	Warnings             []string   `json:"warnings,omitempty"`
}

type GCAction struct {
	Kind       string        `json:"kind"`
	Enabled    bool          `json:"enabled"`
	DryRun     bool          `json:"dry_run"`
	Scope      string        `json:"scope,omitempty"`
	Candidates []GCCandidate `json:"candidates,omitempty"`
	Deleted    []string      `json:"deleted,omitempty"`
	Failed     []string      `json:"failed,omitempty"`
	Commands   []string      `json:"-"`
}

type GCCandidate struct {
	ID     string            `json:"id,omitempty"`
	Name   string            `json:"name,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Reason string            `json:"reason"`
}

func RunGC(ctx context.Context, req GCRunRequest) (GCSummary, error) {
	start := time.Now().UTC()
	if req.Trigger == "" {
		req.Trigger = "admin"
	}
	summary := GCSummary{
		OK:                   true,
		DryRun:               req.DryRun,
		Trigger:              req.Trigger,
		StartedAt:            start.Format(time.RFC3339),
		P2ROnly:              req.Config.GC.P2ROnly,
		ManagedLabel:         req.Config.ManagedLabel,
		ComposeProjectPrefix: req.Config.ComposeProjectPrefix,
	}
	finish := func(err error) (GCSummary, error) {
		now := time.Now().UTC()
		summary.FinishedAt = now.Format(time.RFC3339)
		summary.DurationMS = now.Sub(start).Milliseconds()
		if err != nil {
			summary.OK = false
			summary.Error = err.Error()
		}
		writeErr := WriteGCSummary(req.ScanPath, req.ArtifactRoot, summary)
		if err == nil {
			err = writeErr
		}
		return summary, err
	}
	if req.SkipReason != "" {
		summary.Skipped = true
		summary.SkipReason = req.SkipReason
		return finish(nil)
	}
	if !req.Config.GC.Enabled {
		summary.Skipped = true
		summary.SkipReason = "docker.gc.enabled=false"
		return finish(nil)
	}
	if !req.DryRun && !req.Yes {
		return finish(errString("docker-gc run requires --yes or --dry-run"))
	}
	if !req.Config.GC.P2ROnly && !req.AllowGlobal {
		return finish(errString("global docker-gc requires --allow-global"))
	}
	lock, err := AcquireMaintenanceLock(req.ScanPath, "docker-gc")
	if err != nil {
		summary.Skipped = true
		summary.SkipReason = "maintenance lock unavailable: " + err.Error()
		return finish(nil)
	}
	defer lock.Release()
	summary.LockPath = lock.Path
	if req.Exec == nil {
		req.Exec = executor.New()
	}
	if req.Config.GC.PruneExitedContainers {
		action := runGCListDelete(ctx, req, "container", gcListQueries("container", req.Config), func(id string) []string { return []string{"rm", id} })
		summary.Actions = append(summary.Actions, action)
		summary.Commands = append(summary.Commands, action.Commands...)
	}
	if req.Config.GC.PruneNetworks {
		action := runGCListDelete(ctx, req, "network", gcListQueries("network", req.Config), func(id string) []string { return []string{"network", "rm", id} })
		summary.Actions = append(summary.Actions, action)
		summary.Commands = append(summary.Commands, action.Commands...)
	}
	if req.Config.GC.PruneVolumes {
		action := runGCListDelete(ctx, req, "volume", gcListQueries("volume", req.Config), func(id string) []string { return []string{"volume", "rm", id} })
		summary.Actions = append(summary.Actions, action)
		summary.Commands = append(summary.Commands, action.Commands...)
	}
	if req.Config.GC.PruneImages {
		action := runGCListDelete(ctx, req, "image", gcListQueries("image", req.Config), func(id string) []string { return []string{"image", "rm", id} })
		summary.Actions = append(summary.Actions, action)
		summary.Commands = append(summary.Commands, action.Commands...)
	}
	if req.Config.GC.PruneBuilderCache {
		action := GCAction{Kind: "builder_cache", Enabled: true, DryRun: req.DryRun, Scope: "global_builder_cache_by_age"}
		summary.Commands = append(summary.Commands, "docker builder prune --force --filter until="+req.Config.GC.BuilderCacheUntil)
		if req.DryRun {
			action.Candidates = append(action.Candidates, GCCandidate{Reason: "global_builder_cache_by_age"})
		} else {
			result := req.Exec.Run(ctx, 5*time.Minute, "", nil, "docker", "builder", "prune", "--force", "--filter", "until="+req.Config.GC.BuilderCacheUntil)
			if result.Err != nil {
				action.Failed = append(action.Failed, result.Err.Error())
			} else {
				action.Deleted = append(action.Deleted, strings.TrimSpace(result.Stdout))
			}
		}
		summary.Actions = append(summary.Actions, action)
	}
	return finish(nil)
}

type gcListQuery struct {
	Args    []string
	Command string
	Reason  string
}

func gcListQueries(kind string, cfg config.DockerConfig) []gcListQuery {
	var base []string
	switch kind {
	case "container":
		base = []string{"ps", "-a"}
	case "network":
		base = []string{"network", "ls"}
	case "volume":
		base = []string{"volume", "ls"}
	case "image":
		base = []string{"image", "ls"}
	default:
		return nil
	}
	queries := []gcListQuery{{
		Args:    append(append([]string{}, base...), "--filter", "label="+cfg.ManagedLabel, "--format", "json"),
		Command: "docker " + strings.Join(append(append([]string{}, base...), "--filter", "label="+cfg.ManagedLabel, "--format", "json"), " "),
		Reason:  "managed_label",
	}}
	if strings.TrimSpace(cfg.ComposeProjectPrefix) != "" {
		queries = append(queries, gcListQuery{
			Args:    append(append([]string{}, base...), "--filter", "label=com.docker.compose.project", "--format", "json"),
			Command: "docker " + strings.Join(append(append([]string{}, base...), "--filter", "label=com.docker.compose.project", "--format", "json"), " "),
			Reason:  "compose_project_prefix",
		})
	}
	return queries
}

func runGCListDelete(ctx context.Context, req GCRunRequest, kind string, queries []gcListQuery, deleteArgs func(string) []string) GCAction {
	action := GCAction{Kind: kind, Enabled: true, DryRun: req.DryRun}
	seen := map[string]bool{}
	for _, query := range queries {
		action.Commands = append(action.Commands, query.Command)
		result := req.Exec.Run(ctx, time.Minute, "", nil, "docker", query.Args...)
		if result.Err != nil {
			action.Failed = append(action.Failed, result.Err.Error())
			continue
		}
		for _, candidate := range parseGCCandidates(result.Stdout) {
			if query.Reason == "compose_project_prefix" && !composeProjectOwned(candidate, req.Config.ComposeProjectPrefix) {
				continue
			}
			candidate.Reason = query.Reason
			key := candidateKey(candidate)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			action.Candidates = append(action.Candidates, candidate)
		}
	}
	if req.DryRun {
		return action
	}
	for _, candidate := range action.Candidates {
		id := candidate.ID
		if id == "" {
			id = candidate.Name
		}
		if id == "" {
			continue
		}
		deleted := req.Exec.Run(ctx, time.Minute, "", nil, "docker", deleteArgs(id)...)
		if deleted.Err != nil {
			action.Failed = append(action.Failed, id+": "+deleted.Err.Error())
			continue
		}
		action.Deleted = append(action.Deleted, id)
	}
	return action
}

func composeProjectOwned(candidate GCCandidate, prefix string) bool {
	project := candidate.Labels["com.docker.compose.project"]
	return strings.HasPrefix(project, strings.TrimSpace(prefix)+"_")
}

func candidateKey(candidate GCCandidate) string {
	if candidate.ID != "" {
		return "id:" + candidate.ID
	}
	if candidate.Name != "" {
		return "name:" + candidate.Name
	}
	return ""
}

func parseGCCandidates(raw string) []GCCandidate {
	var candidates []GCCandidate
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal([]byte(line), &payload) != nil {
			continue
		}
		candidate := GCCandidate{Reason: "managed_label"}
		for _, key := range []string{"ID", "Id", "ContainerID"} {
			if value, ok := payload[key].(string); ok && value != "" {
				candidate.ID = value
				break
			}
		}
		for _, key := range []string{"Name", "Names", "Repository"} {
			if value, ok := payload[key].(string); ok && value != "" {
				candidate.Name = value
				break
			}
		}
		candidate.Labels = labelsFromAny(payload["Labels"])
		candidates = append(candidates, candidate)
	}
	return candidates
}

func labelsFromAny(value any) map[string]string {
	labels := map[string]string{}
	switch typed := value.(type) {
	case string:
		for _, item := range strings.Split(typed, ",") {
			key, value, ok := strings.Cut(item, "=")
			if ok {
				labels[strings.TrimSpace(key)] = strings.TrimSpace(value)
			}
		}
	case map[string]any:
		for key, value := range typed {
			labels[key] = strings.TrimSpace(valueString(value))
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

func valueString(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func WriteGCSummary(scanPath, artifactRoot string, summary GCSummary) error {
	path := DockerGCSummaryPath(scanPath)
	if err := writeJSONFile(path, summary); err != nil {
		return err
	}
	if artifactRoot != "" {
		return writeJSONFile(filepath.Join(artifactRoot, "docker_gc_summary.json"), summary)
	}
	return nil
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

type errString string

func (e errString) Error() string {
	return string(e)
}
