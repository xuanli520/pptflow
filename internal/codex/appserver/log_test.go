package appserver

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAppServerLogFilesAreOwnerPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not expose POSIX file permissions")
	}
	path := filepath.Join(t.TempDir(), "nested", "agent.log")
	if err := writeText(path, "first\n"); err != nil {
		t.Fatal(err)
	}
	if err := appendText(path, "second\n"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "first\nsecond\n" {
		t.Fatalf("log contents = %q", contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("log permissions = %o, want owner-private", info.Mode().Perm())
	}
}
