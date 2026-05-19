package maintenance_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/maintenance"
)

func TestTUIStartGCSkipsRecordStateAndThrottleNextCheck(t *testing.T) {
	cfg := config.Default()
	cfg.ScanPath = t.TempDir()
	cfg.Docker.GC.Interval = "24h"

	first, err := maintenance.TryRunOnTUIStart(context.Background(), cfg, nil, 1)
	if err != nil {
		t.Fatalf("first skip failed: %v", err)
	}
	if !first.Skipped || first.SkipReason != "scheduler has active jobs" {
		t.Fatalf("unexpected first summary: %#v", first)
	}
	stateContent, err := maintenanceStateContent(cfg.ScanPath)
	if err != nil {
		t.Fatal(err)
	}
	if stateContent.LastCheckedAt == "" || stateContent.LastStatus != "skipped" {
		t.Fatalf("skipped check should be recorded: %#v", stateContent)
	}

	second, err := maintenance.TryRunOnTUIStart(context.Background(), cfg, nil, 0)
	if err != nil {
		t.Fatalf("second skip failed: %v", err)
	}
	if !second.Skipped || second.SkipReason != "docker.gc.interval has not elapsed" {
		t.Fatalf("last skipped check should throttle startup GC, got %#v", second)
	}
}

func maintenanceStateContent(scanPath string) (maintenance.DockerMaintenanceState, error) {
	var state maintenance.DockerMaintenanceState
	content, err := os.ReadFile(maintenance.DockerMaintenanceStatePath(scanPath))
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(content, &state)
	return state, err
}
