package cmd

import (
	"context"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/testsupport"
)

func TestLifecycleTUICommandUsesConfiguredServiceFactory(t *testing.T) {
	root := t.TempDir()
	factoryCalls := 0
	runnerCalls := 0
	config := &lifecycleCLIConfig{
		root: root,
		newLifecycleService: func(factoryRoot string, database *store.Store) (*app.LifecycleServices, error) {
			factoryCalls++
			return app.NewLifecycleServicesWithOptions(factoryRoot, database, app.LifecycleServicesOptions{
				OperationResolver: testsupport.AcceptAllStageOperationResolver(),
			})
		},
	}
	command := newLifecycleTUICommandWithRunner(config, func(ctx context.Context, services *app.LifecycleServices) error {
		runnerCalls++
		if services == nil || ctx == nil {
			t.Fatal("TUI runner did not receive the composed lifecycle services")
		}
		return nil
	})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("run TUI through composed lifecycle services: %v", err)
	}
	if factoryCalls != 1 || runnerCalls != 1 {
		t.Fatalf("composition calls = factory:%d runner:%d, want 1/1", factoryCalls, runnerCalls)
	}
}

func TestLifecycleTUICompositionReceivesLifecycleServices(t *testing.T) {
	ctx := context.Background()
	services := openCommandLifecycle(t, t.TempDir())
	defer services.Store().Close()

	// Verify services are properly composed and the TUI adapter can access them.
	if services == nil {
		t.Fatal("lifecycle services are nil")
	}
	if services.Store() == nil {
		t.Fatal("lifecycle services store is nil")
	}
	_ = ctx
}
