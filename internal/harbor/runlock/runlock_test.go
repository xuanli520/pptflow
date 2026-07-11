package runlock

import (
	"errors"
	"testing"
)

func TestAcquirePreventsConcurrentWorkspaceRunner(t *testing.T) {
	workspace := t.TempDir()
	first, err := Acquire(workspace, Metadata{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	active, err := IsActive(workspace)
	if err != nil || !active {
		t.Fatalf("active workspace not detected: active=%v err=%v", active, err)
	}
	second, err := Acquire(workspace, Metadata{RunID: "run-2"})
	if second != nil || !errors.Is(err, ErrActive) {
		t.Fatalf("second runner was not rejected: lock=%v err=%v", second, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	active, err = IsActive(workspace)
	if err != nil || active {
		t.Fatalf("released workspace still active: active=%v err=%v", active, err)
	}
}
