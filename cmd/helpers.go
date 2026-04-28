package cmd

import (
	"os"

	"github.com/xuanli520/p2r_tui/internal/config"
	"github.com/xuanli520/p2r_tui/internal/db"
)

func loadConfig(scanPath string) (config.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(cwd, config.Overrides{ScanPath: scanPath})
}

func openStore(cfg config.Config) (*db.Store, error) {
	return db.Open(cfg.DBPath)
}
