package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"golang.org/x/sys/unix"
)

const (
	standardAuthoringLaunchCaptureLockFileName = "capture.lock"
	standardAuthoringLaunchLockPollInterval    = 25 * time.Millisecond
)

// standardAuthoringLaunchOperationLease owns a descriptor rooted at exactly
// one prepared lifecycle operation. All preparation/capture records are
// opened relative to this descriptor, never by reopening a path after a
// symlink check. Its advisory lock serializes the read-only Git acquisition
// and immutable receipt publication for one idempotency key.
type standardAuthoringLaunchOperationLease struct {
	directory *os.File
	lock      *os.File
}

func (lease *standardAuthoringLaunchOperationLease) Close() error {
	if lease == nil {
		return nil
	}
	var result error
	if lease.lock != nil {
		if err := unix.Flock(int(lease.lock.Fd()), unix.LOCK_UN); err != nil && !errors.Is(err, unix.EBADF) {
			result = err
		}
		if err := lease.lock.Close(); err != nil && result == nil {
			result = err
		}
	}
	if lease.directory != nil {
		if err := lease.directory.Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}

// openStandardAuthoringLaunchOperationDirectory creates and opens the fixed
// managed operation directory through directory file descriptors. The
// configured root may be renamed after it is opened, but all descendants stay
// bound to the opened root rather than a subsequently substituted pathname.
func (service *StandardAuthoringLaunchService) openStandardAuthoringLaunchOperationDirectory(operationID string) (*os.File, error) {
	if service == nil || service.core == nil {
		return nil, ErrStandardAuthoringLaunchUnavailable
	}
	if err := store.ValidateUUIDv7(operationID); err != nil {
		return nil, fmt.Errorf("Standard authoring lifecycle operation ID: %w", err)
	}
	if err := service.core.layout.ensureRoot(); err != nil {
		return nil, err
	}
	root, err := standardAuthoringOpenDirectory(service.core.layout.root)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring root: %w", err)
	}
	defer root.Close()
	operations, err := standardAuthoringOpenOrCreateDirectoryAt(root, managedLifecycleOperationsDirectory)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring operations directory: %w", err)
	}
	defer operations.Close()
	operation, err := standardAuthoringOpenOrCreateDirectoryAt(operations, operationID)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring operation directory: %w", err)
	}
	return operation, nil
}

// openExistingStandardAuthoringLaunchOperationDirectory is the read-only
// counterpart used by recovery projections. It deliberately does not create a
// missing root or operation directory while the task board is refreshing.
func (service *StandardAuthoringLaunchService) openExistingStandardAuthoringLaunchOperationDirectory(operationID string) (*os.File, error) {
	if service == nil || service.core == nil {
		return nil, ErrStandardAuthoringLaunchUnavailable
	}
	if err := store.ValidateUUIDv7(operationID); err != nil {
		return nil, fmt.Errorf("Standard authoring lifecycle operation ID: %w", err)
	}
	root, err := standardAuthoringOpenDirectory(service.core.layout.root)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring root: %w", err)
	}
	defer root.Close()
	operations, err := standardAuthoringOpenExistingDirectoryAt(root, managedLifecycleOperationsDirectory)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring operations directory: %w", err)
	}
	defer operations.Close()
	operation, err := standardAuthoringOpenExistingDirectoryAt(operations, operationID)
	if err != nil {
		return nil, fmt.Errorf("open managed Standard authoring operation directory: %w", err)
	}
	return operation, nil
}

func (service *StandardAuthoringLaunchService) lockStandardAuthoringLaunchOperation(ctx context.Context, operationID string) (*standardAuthoringLaunchOperationLease, error) {
	if ctx == nil {
		return nil, errors.New("Standard authoring operation lock context is required")
	}
	directory, err := service.openStandardAuthoringLaunchOperationDirectory(operationID)
	if err != nil {
		return nil, err
	}
	lock, err := standardAuthoringOpenOrCreateRegularFileAt(directory, standardAuthoringLaunchCaptureLockFileName, 0o600)
	if err != nil {
		_ = directory.Close()
		return nil, fmt.Errorf("open Standard authoring operation capture lock: %w", err)
	}
	if err := standardAuthoringFlockContext(ctx, int(lock.Fd())); err != nil {
		_ = lock.Close()
		_ = directory.Close()
		return nil, fmt.Errorf("lock Standard authoring operation capture: %w", err)
	}
	return &standardAuthoringLaunchOperationLease{directory: directory, lock: lock}, nil
}

func standardAuthoringOpenDirectory(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("managed directory must be a clean absolute path")
	}
	fd, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	current := os.NewFile(uintptr(fd), string(filepath.Separator))
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(int(current.Fd()), component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = current.Close()
			return nil, openErr
		}
		next := os.NewFile(uintptr(nextFD), component)
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return nil, closeErr
		}
		current = next
	}
	return current, nil
}

func standardAuthoringOpenOrCreateDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if parent == nil || name == "" || name == "." || name == ".." {
		return nil, errors.New("invalid managed directory")
	}
	fd := int(parent.Fd())
	if err := unix.Mkdirat(fd, name, 0o750); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	childFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(childFD), name)
	if info, err := child.Stat(); err != nil || !info.IsDir() {
		_ = child.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("managed path is not a directory")
	}
	if err := parent.Sync(); err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func standardAuthoringOpenExistingDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if parent == nil || name == "" || name == "." || name == ".." {
		return nil, errors.New("invalid managed directory")
	}
	childFD, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(childFD), name)
	if info, err := child.Stat(); err != nil || !info.IsDir() {
		_ = child.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("managed path is not a directory")
	}
	return child, nil
}

func standardAuthoringOpenOrCreateRegularFileAt(directory *os.File, name string, mode uint32) (*os.File, error) {
	if directory == nil || name == "" || name == "." || name == ".." {
		return nil, errors.New("invalid managed file")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("managed path is not a regular file")
	}
	return file, nil
}

func standardAuthoringFlockContext(ctx context.Context, fd int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(standardAuthoringLaunchLockPollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func standardAuthoringReadNewImmutableFileAt(directory *os.File, name string, maximum int64) ([]byte, bool, error) {
	if directory == nil || maximum < 1 || name == "" || name == "." || name == ".." {
		return nil, false, errors.New("invalid managed immutable file request")
	}
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximum {
		if err != nil {
			return nil, false, err
		}
		return nil, false, errors.New("managed immutable file is not an eligible regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) != info.Size() || int64(len(raw)) > maximum {
		return nil, false, errors.New("managed immutable file changed while reading")
	}
	return raw, true, nil
}

// standardAuthoringWriteNewImmutableFileAt publishes bytes with a hard-link
// from a fully synced, descriptor-rooted temporary file. Link publication
// never replaces an existing record, so a malformed or tampered record is
// rejected by the reader rather than overwritten during recovery.
func standardAuthoringWriteNewImmutableFileAt(directory *os.File, name string, value []byte, mode uint32) error {
	if directory == nil || len(value) == 0 || name == "" || name == "." || name == ".." {
		return errors.New("invalid managed immutable file publication")
	}
	temporaryName, err := standardAuthoringTemporaryFileName(name)
	if err != nil {
		return err
	}
	directoryFD := int(directory.Fd())
	fd, err := unix.Openat(directoryFD, temporaryName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), temporaryName)
	defer func() {
		_ = file.Close()
		_ = unix.Unlinkat(directoryFD, temporaryName, 0)
	}()
	written, err := file.Write(value)
	if err != nil {
		return err
	}
	if written != len(value) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Linkat(directoryFD, temporaryName, directoryFD, name, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return os.ErrExist
		}
		return err
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	if err := unix.Unlinkat(directoryFD, temporaryName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return directory.Sync()
}

func standardAuthoringTemporaryFileName(target string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "." + target + ".new-" + hex.EncodeToString(random[:]), nil
}
