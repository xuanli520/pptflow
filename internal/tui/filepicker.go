package tui

import (
	"strings"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

func attachRunConfigDoc(cfg config.Config, taskID, path string) (taskdocs.Document, error) {
	return taskdocs.Attach(cfg.ScanPath, taskID, strings.TrimSpace(path), "attached from p2r TUI run config", "p2r-tui", cfg.Docs)
}
