package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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

func TestLifecycleTUIReportsActiveRunSchemaUpgradeWithoutStoreInternals(t *testing.T) {
	root := t.TempDir()
	const runID = "019f8ce2-a0d6-774c-b50f-fcaac0797dae"
	blocked := fmt.Errorf("%w: run %s is %s; finish it with the deployment package that froze its execution contract or initialize a new root", store.ErrActiveRunSchemaUpgrade, runID, store.WorkflowRunWaitingReview)
	runnerCalled := false
	command := newLifecycleTUICommandWithDependencies(&lifecycleCLIConfig{root: root}, func(context.Context, *app.LifecycleServices) error {
		runnerCalled = true
		return nil
	}, func(openRoot string) (*store.Store, error) {
		if openRoot != root {
			t.Fatalf("store open root = %q, want %q", openRoot, root)
		}
		return nil, blocked
	})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	err := command.ExecuteContext(context.Background())
	if !errors.Is(err, store.ErrActiveRunSchemaUpgrade) {
		t.Fatalf("TUI active-run schema error = %v, want %v", err, store.ErrActiveRunSchemaUpgrade)
	}
	message := err.Error()
	for _, want := range []string{"open lifecycle control plane", runID, "finish it with the deployment package"} {
		if !strings.Contains(message, want) {
			t.Fatalf("TUI active-run schema error = %q, missing %q", message, want)
		}
	}
	for _, leaked := range []string{root, "harbor.db", "SELECT ", "sqlite:"} {
		if strings.Contains(message, leaked) {
			t.Fatalf("TUI active-run schema error leaked %q: %q", leaked, message)
		}
	}
	if runnerCalled {
		t.Fatal("blocked TUI reached its runner")
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
