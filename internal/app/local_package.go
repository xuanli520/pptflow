package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/codeedge"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const localPackageReceiptFormat = "harbor.local-package-receipt.v3"

// codeEdgeLocalPackageReceipt pins the exact final-compliance authorization
// consumed by a CodeEdge package. The full typed authorization is retained so
// a crash replay can prove the same immutable decision, rather than accepting
// a caller's current summary or recomputing from mutable deployment state.
type codeEdgeLocalPackageReceipt struct {
	ComplianceRecordID       string                             `json:"compliance_record_id"`
	RunID                    string                             `json:"run_id"`
	DecisionFingerprint      workflowkit.Fingerprint            `json:"decision_fingerprint"`
	AuthorizationFingerprint workflowkit.Fingerprint            `json:"authorization_fingerprint"`
	Authorization            codeedge.LocalPackageAuthorization `json:"authorization"`
}

type localPackageReceipt struct {
	Format         string                       `json:"format"`
	TaskID         string                       `json:"task_id"`
	RevisionID     string                       `json:"revision_id"`
	TaskDigest     string                       `json:"task_digest"`
	ReleaseVersion string                       `json:"release_version"`
	CodeEdge       *codeEdgeLocalPackageReceipt `json:"codeedge,omitempty"`
	Package        workflowruntime.ObjectRef    `json:"package"`
	CreatedAt      time.Time                    `json:"created_at"`
}

func validateLocalReleaseVersion(value string) error {
	if value = strings.TrimSpace(value); value == "" {
		return fmt.Errorf("local release version is required")
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') && character != '.' && character != '-' && character != '_' {
			return fmt.Errorf("local release version contains an unsafe character")
		}
	}
	return nil
}

func packageManagedSnapshot(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, snapshot, outputDirectory, taskName string, createdAt time.Time) (object workflowruntime.ObjectRef, packagePath string, err error) {
	if ctx == nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return workflowruntime.ObjectRef{}, "", err
	}
	if err := taskpolicy.ValidateManagedSnapshotV2(snapshot); err != nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("validate package snapshot: %w", err)
	}
	if strings.TrimSpace(taskName) == "" {
		taskName = "task"
	}
	taskName = safePackageRoot(taskName)
	if err := os.MkdirAll(filepath.Dir(outputDirectory), 0o750); err != nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("create local package parent: %w", err)
	}
	if err := os.Mkdir(outputDirectory, 0o750); err != nil {
		if os.IsExist(err) {
			return workflowruntime.ObjectRef{}, "", fmt.Errorf("local package directory already exists: %s", outputDirectory)
		}
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("create local package directory: %w", err)
	}
	// A failed first write leaves no reusable package state. Cleaning only the
	// directory created by this invocation makes a retry safe while preserving
	// a crash-surviving directory that already has an immutable receipt.
	defer func() {
		if err != nil {
			_ = os.RemoveAll(outputDirectory)
		}
	}()
	temporary, err := os.CreateTemp(outputDirectory, ".package-*.zip")
	if err != nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("create package temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	zipWriter := zip.NewWriter(temporary)
	copyErr := writeCanonicalPackageZip(ctx, zipWriter, snapshot, taskName, createdAt)
	if closeErr := zipWriter.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if syncErr := temporary.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := temporary.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return workflowruntime.ObjectRef{}, "", copyErr
	}
	input, err := os.Open(temporaryPath)
	if err != nil {
		return workflowruntime.ObjectRef{}, "", err
	}
	object, putErr := objects.Put(ctx, input)
	closeErr := input.Close()
	if putErr != nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("store local package object: %w", putErr)
	}
	if closeErr != nil {
		return workflowruntime.ObjectRef{}, "", closeErr
	}
	objectPath, err := objects.ObjectPath(object)
	if err != nil {
		return workflowruntime.ObjectRef{}, "", err
	}
	packagePath = filepath.Join(outputDirectory, "package.zip")
	if err := os.Link(objectPath, packagePath); err != nil {
		return workflowruntime.ObjectRef{}, "", fmt.Errorf("link immutable local package: %w", err)
	}
	return object, packagePath, nil
}

// existingLocalPackage verifies a crash-surviving receipt before allowing a
// release operation to reuse it. A receipt is valid only when its immutable
// object, hard-linked package path, and task/revision/version binding agree.
func existingLocalPackage(ctx context.Context, objects *workflowruntime.ArtifactObjectStore, outputDirectory string, expected localPackageReceipt) (workflowruntime.ObjectRef, string, bool, error) {
	if ctx == nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return workflowruntime.ObjectRef{}, "", false, err
	}
	receiptPath := filepath.Join(outputDirectory, "receipt.json")
	raw, err := os.ReadFile(receiptPath)
	if errors.Is(err, os.ErrNotExist) {
		return workflowruntime.ObjectRef{}, "", false, nil
	}
	if err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("read local package receipt: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var receipt localPackageReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("decode local package receipt: %w", err)
	}
	if receipt.Format != localPackageReceiptFormat || receipt.TaskID != expected.TaskID || receipt.RevisionID != expected.RevisionID || receipt.TaskDigest != expected.TaskDigest || receipt.ReleaseVersion != expected.ReleaseVersion || !sameCodeEdgeLocalPackageReceipt(receipt.CodeEdge, expected.CodeEdge) {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("local package receipt does not match requested release")
	}
	if receipt.CodeEdge != nil {
		if err := receipt.CodeEdge.Validate(); err != nil {
			return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("validate CodeEdge local package authorization: %w", err)
		}
	}
	if err := receipt.Package.Validate(); err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("validate local package receipt object: %w", err)
	}
	if err := objects.Verify(ctx, receipt.Package); err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("verify local package object: %w", err)
	}
	packagePath := filepath.Join(outputDirectory, "package.zip")
	packageInfo, err := os.Lstat(packagePath)
	if err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("inspect local package path: %w", err)
	}
	if packageInfo.Mode()&os.ModeSymlink != 0 || !packageInfo.Mode().IsRegular() {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("local package path is not a regular file")
	}
	objectPath, err := objects.ObjectPath(receipt.Package)
	if err != nil {
		return workflowruntime.ObjectRef{}, "", false, err
	}
	objectInfo, err := os.Stat(objectPath)
	if err != nil {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("inspect local package object: %w", err)
	}
	if !os.SameFile(packageInfo, objectInfo) {
		return workflowruntime.ObjectRef{}, "", false, fmt.Errorf("local package path is not linked to its immutable object")
	}
	return receipt.Package, packagePath, true, nil
}

func (receipt codeEdgeLocalPackageReceipt) Validate() error {
	if strings.TrimSpace(receipt.ComplianceRecordID) == "" || strings.TrimSpace(receipt.RunID) == "" {
		return fmt.Errorf("CodeEdge compliance record and Run identities are required")
	}
	if err := receipt.DecisionFingerprint.Validate(); err != nil {
		return fmt.Errorf("CodeEdge decision fingerprint: %w", err)
	}
	if err := receipt.AuthorizationFingerprint.Validate(); err != nil {
		return fmt.Errorf("CodeEdge authorization fingerprint: %w", err)
	}
	if err := receipt.Authorization.Validate(); err != nil {
		return err
	}
	decisionFingerprint, err := receipt.Authorization.Decision.Fingerprint()
	if err != nil {
		return err
	}
	if decisionFingerprint != receipt.DecisionFingerprint || receipt.Authorization.DecisionFingerprint != receipt.DecisionFingerprint {
		return fmt.Errorf("CodeEdge local package decision fingerprint does not match authorization")
	}
	authorizationFingerprint, err := receipt.Authorization.Fingerprint()
	if err != nil {
		return err
	}
	if authorizationFingerprint != receipt.AuthorizationFingerprint {
		return fmt.Errorf("CodeEdge local package authorization fingerprint does not match authorization")
	}
	return nil
}

func sameCodeEdgeLocalPackageReceipt(left, right *codeEdgeLocalPackageReceipt) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.ComplianceRecordID == right.ComplianceRecordID &&
		left.RunID == right.RunID &&
		left.DecisionFingerprint == right.DecisionFingerprint &&
		left.AuthorizationFingerprint == right.AuthorizationFingerprint
}

func writeCanonicalPackageZip(ctx context.Context, writer *zip.Writer, snapshot, taskName string, createdAt time.Time) error {
	for _, file := range taskpolicy.CanonicalFiles() {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(snapshot, filepath.FromSlash(file.Path))
		input, err := os.Open(path)
		if os.IsNotExist(err) && file.Environment {
			continue
		}
		if err != nil {
			return fmt.Errorf("open package input %s: %w", file.Path, err)
		}
		info, err := input.Stat()
		if err != nil {
			_ = input.Close()
			return err
		}
		if !info.Mode().IsRegular() {
			_ = input.Close()
			return fmt.Errorf("package input is not regular: %s", file.Path)
		}
		header := &zip.FileHeader{Name: filepath.ToSlash(filepath.Join(taskName, file.Path)), Method: zip.Deflate}
		header.SetMode(file.Mode)
		header.SetModTime(createdAt.UTC())
		output, err := writer.CreateHeader(header)
		if err == nil {
			err = copyPackageBytes(ctx, output, input)
		}
		closeErr := input.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func copyPackageBytes(ctx context.Context, output io.Writer, input io.Reader) error {
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := input.Read(buffer)
		if read > 0 {
			written, err := output.Write(buffer[:read])
			if err != nil {
				return err
			}
			if written != read {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if read == 0 {
			return fmt.Errorf("package input made no progress")
		}
	}
}

func safePackageRoot(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "task"
	}
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '-' || character == '_' {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
	}
	if result.Len() == 0 {
		return "task"
	}
	return result.String()
}
