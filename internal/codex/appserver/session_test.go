package appserver

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/xuanli520/pptflow/internal/executor"
)

func TestWaitReturnsAfterCompleteClosesOpenStreams(t *testing.T) {
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	defer stdoutWriter.Close()
	defer stderrWriter.Close()

	session := &appServerCodexReviewSession{
		done:        make(chan struct{}),
		responses:   map[int]chan appServerRPCMessage{},
		deltas:      map[string]string{},
		deltaLogged: map[string]bool{},
		stdoutPipe:  stdoutReader,
		stderrPipe:  stderrReader,
	}
	session.wg.Add(2)
	go func() {
		defer session.wg.Done()
		session.readStdout(stdoutReader)
	}()
	go func() {
		defer session.wg.Done()
		session.readStderr(stderrReader)
	}()

	session.complete(executor.Result{Command: "fake app-server"}, nil)

	done := make(chan error, 1)
	go func() {
		_, err := session.Wait(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return after complete closed app-server streams")
	}
}
