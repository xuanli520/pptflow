package browser_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	browserpkg "github.com/xuanli520/p2r_tui/internal/browser"
	"github.com/xuanli520/p2r_tui/internal/executor"
)

func TestPlaywrightWrapperReturnsObservationWhenProcessReportsError(t *testing.T) {
	runner := browserpkg.NewPlaywrightWrapper(wrapperErrorRunner{}, "node", browserpkg.Policy{
		ArtifactRoot:      t.TempDir(),
		DisableScreenshot: true,
		AllowlistOrigins:  []string{"http://127.0.0.1:3000"},
		StorageStatePath:  "",
		LastURLPath:       "",
		FormStatePath:     "",
	})
	observation, err := runner.Run(context.Background(), browserpkg.Action{Name: "click_navigation"}, time.Second)
	if err != nil {
		t.Fatalf("expected valid stdout observation to suppress process error, got %v", err)
	}
	if !observation.OK || observation.CurrentURL != "http://127.0.0.1:3000/register" {
		t.Fatalf("unexpected observation: %#v", observation)
	}
}

type wrapperErrorRunner struct{}

func (wrapperErrorRunner) LookPath(name string) (string, error) {
	return name, nil
}

func (wrapperErrorRunner) Run(context.Context, time.Duration, string, []string, string, ...string) executor.Result {
	return executor.Result{
		Stdout: `{"action":"click_navigation","ok":true,"current_url":"http://127.0.0.1:3000/register","title":"Register","visible_text":"Register Username Password"}`,
		Err:    errors.New("exit status 1"),
	}
}

func (wrapperErrorRunner) RunStreamingWithOutput(context.Context, time.Duration, string, []string, io.Writer, executor.OutputCallback, string, ...string) executor.Result {
	return executor.Result{}
}
