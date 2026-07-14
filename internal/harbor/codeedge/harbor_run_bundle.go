package codeedge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/purplevoid/harbor-factory/internal/harbor/secretscan"
	"github.com/purplevoid/harbor-factory/internal/harbor/taskpolicy"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

const (
	// HarborRunBundleV018Format identifies one self-contained, canonical capture
	// of a local Harbor 0.18.0 job directory. The bundle is a Harbor Flow-owned
	// evidence format, not an upstream Harbor file format.
	HarborRunBundleV018Format = "harbor.run-bundle.v0.18"
	// HarborRunBundleV018Version is incremented when bundle semantics change.
	HarborRunBundleV018Version = "1"
)

var (
	// ErrInvalidHarborRunBundle marks malformed, non-canonical, incomplete, or
	// untrusted local Harbor run evidence.
	ErrInvalidHarborRunBundle = errors.New("CodeEdge Harbor 0.18 run bundle: invalid")
	// ErrHarborRunBundleSecret marks a text secret detected before local Harbor
	// output is admitted to immutable artifact storage.
	ErrHarborRunBundleSecret = errors.New("CodeEdge Harbor 0.18 run bundle: secret detected")
)

// HarborRunBundleCaptureRequest supplies only controlled local locations and
// frozen identities. JobDirectory and MaterializedTaskRoot are deliberately
// not serialized into the resulting bundle.
type HarborRunBundleCaptureRequest struct {
	JobDirectory             string                    `json:"-"`
	MaterializedTaskRoot     string                    `json:"-"`
	FrozenTaskSnapshotDigest workflowkit.SubjectDigest `json:"-"`
	HarborCLI                HarborCLIIdentity         `json:"-"`
}

// HarborRunBundlePathSummaryV018 records the stable relative path, byte count,
// and content digest for one captured regular file. It never carries a host
// absolute path or a generated trial-directory identity as a durable ID.
type HarborRunBundlePathSummaryV018 struct {
	Path          string                  `json:"path"`
	Size          int64                   `json:"size"`
	ContentDigest workflowkit.Fingerprint `json:"content_digest"`
}

// HarborRunBundleFileV018 holds one captured file's bytes as standard base64.
// The matching Paths entry owns the path summary and digest.
type HarborRunBundleFileV018 struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
}

// HarborRunBundleV018 is one canonical JSON evidence artifact. It contains
// every regular non-symlink file below the controlled local job directory so
// later readers do not need to revisit a mutable Harbor working directory.
//
// SourceTaskSnapshotDigest and MaterializedTaskRootV2Digest intentionally have
// distinct names even though Capture requires their V2 values to agree. Neither
// is a Harbor Trial task_checksum or a Harbor lock task.digest.
type HarborRunBundleV018 struct {
	Format                       string                           `json:"format"`
	Version                      string                           `json:"version"`
	HarborCLI                    HarborCLIIdentity                `json:"harbor_cli"`
	SourceTaskSnapshotDigest     workflowkit.SubjectDigest        `json:"source_task_snapshot_digest"`
	MaterializedTaskRootV2Digest workflowkit.SubjectDigest        `json:"materialized_task_root_v2_digest"`
	Paths                        []HarborRunBundlePathSummaryV018 `json:"paths"`
	Files                        []HarborRunBundleFileV018        `json:"files"`
}

// CaptureHarborRunBundleV018 validates frozen task input, verifies that the
// Harbor config references exactly that controlled materialized root, scans
// text evidence for secrets, then captures every regular file into one bundle.
func CaptureHarborRunBundleV018(request HarborRunBundleCaptureRequest) (HarborRunBundleV018, error) {
	if err := request.validate(); err != nil {
		return HarborRunBundleV018{}, err
	}
	jobDirectory, err := harborRunBundleControlledDirectory("Harbor job directory", request.JobDirectory)
	if err != nil {
		return HarborRunBundleV018{}, err
	}
	taskRoot, err := harborRunBundleControlledDirectory("materialized task root", request.MaterializedTaskRoot)
	if err != nil {
		return HarborRunBundleV018{}, err
	}
	materializedDigest, err := taskpolicy.ComputeManagedTaskDigestV2(taskRoot)
	if err != nil {
		return HarborRunBundleV018{}, fmt.Errorf("%w: compute materialized task V2 digest: %v", ErrInvalidHarborRunBundle, err)
	}
	if materializedDigest != string(request.FrozenTaskSnapshotDigest) {
		return HarborRunBundleV018{}, fmt.Errorf("%w: materialized task root V2 digest does not equal frozen task snapshot digest", ErrInvalidHarborRunBundle)
	}
	if err := harborRunBundleValidateJobConfigTaskRoot(filepath.Join(jobDirectory, "config.json"), taskRoot); err != nil {
		return HarborRunBundleV018{}, err
	}

	paths, files, err := harborRunBundleCaptureFiles(jobDirectory)
	if err != nil {
		return HarborRunBundleV018{}, err
	}
	bundle := HarborRunBundleV018{
		Format:                       HarborRunBundleV018Format,
		Version:                      HarborRunBundleV018Version,
		HarborCLI:                    request.HarborCLI,
		SourceTaskSnapshotDigest:     request.FrozenTaskSnapshotDigest,
		MaterializedTaskRootV2Digest: workflowkit.SubjectDigest(materializedDigest),
		Paths:                        paths,
		Files:                        files,
	}
	if _, err := InspectHarborRunBundleV018(bundle); err != nil {
		return HarborRunBundleV018{}, err
	}
	return bundle, nil
}

func (request HarborRunBundleCaptureRequest) validate() error {
	if err := taskpolicy.ValidateV2TaskDigest(string(request.FrozenTaskSnapshotDigest)); err != nil {
		return fmt.Errorf("%w: frozen task snapshot digest: %v", ErrInvalidHarborRunBundle, err)
	}
	if err := request.HarborCLI.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor CLI identity: %v", ErrInvalidHarborRunBundle, err)
	}
	return nil
}

// CanonicalJSON returns the sole allowed serialized representation of bundle.
func (bundle HarborRunBundleV018) CanonicalJSON() ([]byte, error) {
	if err := bundle.validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return nil, fmt.Errorf("%w: encode canonical bundle: %v", ErrInvalidHarborRunBundle, err)
	}
	return encoded, nil
}

// ParseHarborRunBundleV018 strictly accepts only the canonical JSON encoding.
// It rejects duplicate keys, unknown fields, trailing JSON, altered file
// digests, unsafe paths, and text secrets before exposing any evidence bytes.
func ParseHarborRunBundleV018(raw []byte) (HarborRunBundleV018, error) {
	if err := harborRunBundleRejectDuplicateJSONKeys(raw); err != nil {
		return HarborRunBundleV018{}, fmt.Errorf("%w: duplicate or malformed JSON: %v", ErrInvalidHarborRunBundle, err)
	}
	var document harborRunBundleV018Document
	if err := harborRunBundleStrictDecode(raw, &document); err != nil {
		return HarborRunBundleV018{}, fmt.Errorf("%w: decode bundle: %v", ErrInvalidHarborRunBundle, err)
	}
	bundle := HarborRunBundleV018(document)
	canonical, err := bundle.CanonicalJSON()
	if err != nil {
		return HarborRunBundleV018{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return HarborRunBundleV018{}, fmt.Errorf("%w: bundle JSON is not canonical", ErrInvalidHarborRunBundle)
	}
	return bundle, nil
}

// UnmarshalJSON retains the strict/canonical semantics for direct json use.
func (bundle *HarborRunBundleV018) UnmarshalJSON(raw []byte) error {
	if bundle == nil {
		return fmt.Errorf("%w: nil bundle", ErrInvalidHarborRunBundle)
	}
	parsed, err := ParseHarborRunBundleV018(raw)
	if err != nil {
		return err
	}
	*bundle = parsed
	return nil
}

type harborRunBundleV018Document HarborRunBundleV018

func (bundle HarborRunBundleV018) validate() error {
	if bundle.Format != HarborRunBundleV018Format || bundle.Version != HarborRunBundleV018Version {
		return fmt.Errorf("%w: unsupported format/version %q/%q", ErrInvalidHarborRunBundle, bundle.Format, bundle.Version)
	}
	if err := bundle.HarborCLI.Validate(); err != nil {
		return fmt.Errorf("%w: Harbor CLI identity: %v", ErrInvalidHarborRunBundle, err)
	}
	if err := taskpolicy.ValidateV2TaskDigest(string(bundle.SourceTaskSnapshotDigest)); err != nil {
		return fmt.Errorf("%w: source task snapshot digest: %v", ErrInvalidHarborRunBundle, err)
	}
	if err := taskpolicy.ValidateV2TaskDigest(string(bundle.MaterializedTaskRootV2Digest)); err != nil {
		return fmt.Errorf("%w: materialized task root V2 digest: %v", ErrInvalidHarborRunBundle, err)
	}
	if bundle.SourceTaskSnapshotDigest != bundle.MaterializedTaskRootV2Digest {
		return fmt.Errorf("%w: frozen and materialized V2 task digests differ", ErrInvalidHarborRunBundle)
	}
	if bundle.Paths == nil || bundle.Files == nil || len(bundle.Paths) == 0 || len(bundle.Paths) != len(bundle.Files) {
		return fmt.Errorf("%w: paths and files must be equally populated", ErrInvalidHarborRunBundle)
	}
	for index := range bundle.Paths {
		summary := bundle.Paths[index]
		file := bundle.Files[index]
		if err := harborRunBundleValidatePath(summary.Path); err != nil {
			return err
		}
		if summary.Path != file.Path {
			return fmt.Errorf("%w: path summary and content path differ at index %d", ErrInvalidHarborRunBundle, index)
		}
		if summary.Size < 0 {
			return fmt.Errorf("%w: negative file size for %q", ErrInvalidHarborRunBundle, summary.Path)
		}
		if err := summary.ContentDigest.Validate(); err != nil {
			return fmt.Errorf("%w: file %q content digest: %v", ErrInvalidHarborRunBundle, summary.Path, err)
		}
		if index > 0 && bundle.Paths[index-1].Path >= summary.Path {
			return fmt.Errorf("%w: paths are not strictly sorted", ErrInvalidHarborRunBundle)
		}
		content, err := base64.StdEncoding.DecodeString(file.ContentBase64)
		if err != nil || base64.StdEncoding.EncodeToString(content) != file.ContentBase64 {
			return fmt.Errorf("%w: file %q has non-canonical base64 content", ErrInvalidHarborRunBundle, summary.Path)
		}
		if int64(len(content)) != summary.Size || workflowkit.SHA256Fingerprint(content) != summary.ContentDigest {
			return fmt.Errorf("%w: file %q does not match its path summary", ErrInvalidHarborRunBundle, summary.Path)
		}
		if findings := secretscan.ScanBytes(summary.Path, content); len(findings) > 0 {
			return fmt.Errorf("%w: %s", ErrHarborRunBundleSecret, secretscan.Summary(findings, 3))
		}
	}
	return nil
}

func harborRunBundleControlledDirectory(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%w: %s is required", ErrInvalidHarborRunBundle, label)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("%w: resolve %s: %v", ErrInvalidHarborRunBundle, label, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("%w: inspect %s: %v", ErrInvalidHarborRunBundle, label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("%w: %s must be a non-symlink directory", ErrInvalidHarborRunBundle, label)
	}
	return abs, nil
}

func harborRunBundleCaptureFiles(root string) ([]HarborRunBundlePathSummaryV018, []HarborRunBundleFileV018, error) {
	type capturedFile struct {
		path    string
		content []byte
	}
	files := make([]capturedFile, 0)
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink is not allowed in Harbor job directory: %s", ErrInvalidHarborRunBundle, relative)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: non-regular file is not allowed in Harbor job directory: %s", ErrInvalidHarborRunBundle, relative)
		}
		content, err := harborRunBundleReadRegularFile(current)
		if err != nil {
			return fmt.Errorf("%w: read Harbor job file %s: %v", ErrInvalidHarborRunBundle, relative, err)
		}
		if findings := secretscan.ScanBytes(relative, content); len(findings) > 0 {
			return fmt.Errorf("%w: %s", ErrHarborRunBundleSecret, secretscan.Summary(findings, 3))
		}
		files = append(files, capturedFile{path: relative, content: content})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(left, right int) bool { return files[left].path < files[right].path })
	paths := make([]HarborRunBundlePathSummaryV018, 0, len(files))
	contents := make([]HarborRunBundleFileV018, 0, len(files))
	for _, file := range files {
		paths = append(paths, HarborRunBundlePathSummaryV018{
			Path: file.path, Size: int64(len(file.content)), ContentDigest: workflowkit.SHA256Fingerprint(file.content),
		})
		contents = append(contents, HarborRunBundleFileV018{Path: file.path, ContentBase64: base64.StdEncoding.EncodeToString(file.content)})
	}
	return paths, contents, nil
}

func harborRunBundleReadRegularFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("path is not a regular non-symlink file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("file changed while opening")
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != int64(len(content)) {
		return nil, errors.New("file changed while reading")
	}
	return content, nil
}

func harborRunBundleValidateJobConfigTaskRoot(configPath, taskRoot string) error {
	raw, err := harborRunBundleReadRegularFile(configPath)
	if err != nil {
		return fmt.Errorf("%w: read Harbor job config: %v", ErrInvalidHarborRunBundle, err)
	}
	root, err := harborRunBundleJSONObject(raw, "Harbor job config")
	if err != nil {
		return err
	}
	tasks, err := harborRunBundleRequiredArray(root, "tasks", "Harbor job config")
	if err != nil || len(tasks) != 1 {
		if err == nil {
			err = errors.New("must contain exactly one task")
		}
		return fmt.Errorf("%w: Harbor job config.tasks: %v", ErrInvalidHarborRunBundle, err)
	}
	if rawDatasets, present := root["datasets"]; present && !harborRunBundleJSONNull(rawDatasets) {
		datasets, parseErr := harborRunBundleArray(rawDatasets, "Harbor job config.datasets")
		if parseErr != nil || len(datasets) != 0 {
			if parseErr == nil {
				parseErr = errors.New("must be empty when a controlled task path is configured")
			}
			return fmt.Errorf("%w: Harbor job config.datasets: %v", ErrInvalidHarborRunBundle, parseErr)
		}
	}
	task, err := harborRunBundleJSONObject(tasks[0], "Harbor job config.tasks[0]")
	if err != nil {
		return err
	}
	configuredPath, err := harborRunBundleRequiredString(task, "path", "Harbor job config.tasks[0]")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(configuredPath) || filepath.Clean(configuredPath) != configuredPath {
		return fmt.Errorf("%w: Harbor job config task path must be canonical and absolute", ErrInvalidHarborRunBundle)
	}
	if configuredPath != taskRoot {
		return fmt.Errorf("%w: Harbor job config task path does not point to the controlled materialized root", ErrInvalidHarborRunBundle)
	}
	return nil
}

func harborRunBundleValidatePath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) || path.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") || value == ".." {
		return fmt.Errorf("%w: unsafe bundle path %q", ErrInvalidHarborRunBundle, value)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("%w: bundle path contains a control character", ErrInvalidHarborRunBundle)
		}
	}
	return nil
}

func harborRunBundleStrictDecode(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func harborRunBundleRejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := harborRunBundleScanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func harborRunBundleScanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := harborRunBundleScanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			if err != nil {
				return err
			}
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := harborRunBundleScanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			if err != nil {
				return err
			}
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func harborRunBundleJSONObject(raw []byte, label string) (map[string]json.RawMessage, error) {
	if err := harborRunBundleRejectDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("%w: %s duplicate or malformed JSON: %v", ErrInvalidHarborRunBundle, label, err)
	}
	if harborRunBundleJSONNull(raw) {
		return nil, fmt.Errorf("%w: %s must be an object", ErrInvalidHarborRunBundle, label)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("object is required")
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidHarborRunBundle, label, err)
	}
	return object, nil
}

func harborRunBundleArray(raw []byte, label string) ([]json.RawMessage, error) {
	if harborRunBundleJSONNull(raw) {
		return nil, fmt.Errorf("%w: %s must be an array", ErrInvalidHarborRunBundle, label)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || values == nil {
		if err == nil {
			err = errors.New("array is required")
		}
		return nil, fmt.Errorf("%w: %s: %v", ErrInvalidHarborRunBundle, label, err)
	}
	return values, nil
}

func harborRunBundleRequiredArray(object map[string]json.RawMessage, key, label string) ([]json.RawMessage, error) {
	raw, present := object[key]
	if !present {
		return nil, fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborRunBundle, label, key)
	}
	return harborRunBundleArray(raw, label+"."+key)
}

func harborRunBundleRequiredString(object map[string]json.RawMessage, key, label string) (string, error) {
	raw, present := object[key]
	if !present || harborRunBundleJSONNull(raw) {
		return "", fmt.Errorf("%w: %s.%s is required", ErrInvalidHarborRunBundle, label, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%w: %s.%s must be a string", ErrInvalidHarborRunBundle, label, key)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%w: %s.%s must not be empty", ErrInvalidHarborRunBundle, label, key)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", fmt.Errorf("%w: %s.%s contains a control character", ErrInvalidHarborRunBundle, label, key)
		}
	}
	return value, nil
}

func harborRunBundleJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
