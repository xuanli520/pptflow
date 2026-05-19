package taskdocs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xuanli520/p2r_tui/internal/config"
)

const ManifestVersion = 1

type Manifest struct {
	ManifestVersion int        `json:"manifest_version"`
	TaskID          string     `json:"task_id"`
	Docs            []Document `json:"docs"`
}

type Document struct {
	DocID            string   `json:"doc_id"`
	OriginalName     string   `json:"original_name"`
	StoredName       string   `json:"stored_name"`
	SourcePath       string   `json:"source_path"`
	SHA256           string   `json:"sha256"`
	SizeBytes        int64    `json:"size_bytes"`
	MIMEOrExtension  string   `json:"mime_or_extension"`
	TextIncluded     bool     `json:"text_included"`
	SkipReason       string   `json:"skip_reason,omitempty"`
	AttachedAt       string   `json:"attached_at"`
	AttachedBy       string   `json:"attached_by"`
	Notes            string   `json:"notes,omitempty"`
	IncludedInStages []string `json:"included_in_stages"`
}

type ContextResult struct {
	Text string     `json:"text"`
	Docs []Document `json:"docs"`
}

func Attach(scanPath, taskID, sourcePath, note, attachedBy string, limits config.DocsConfig) (Document, error) {
	sourceAbs, err := filepath.Abs(filepath.Clean(sourcePath))
	if err != nil {
		return Document{}, err
	}
	info, err := os.Stat(sourceAbs)
	if err != nil {
		return Document{}, err
	}
	if info.IsDir() {
		return Document{}, fmt.Errorf("attachment path is a directory: %s", sourceAbs)
	}
	if limits.MaxAttachmentBytes > 0 && info.Size() > limits.MaxAttachmentBytes {
		return Document{}, fmt.Errorf("attachment exceeds max_attachment_bytes (%d): %s", limits.MaxAttachmentBytes, sourceAbs)
	}
	store := StoreDir(scanPath, taskID)
	filesDir := filepath.Join(store, "files")
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return Document{}, err
	}
	storeAbs, _ := filepath.Abs(store)
	if inside(sourceAbs, storeAbs) {
		return Document{}, fmt.Errorf("attachment source is inside the managed docs store: %s", sourceAbs)
	}
	content, err := os.ReadFile(sourceAbs)
	if err != nil {
		return Document{}, err
	}
	if int64(len(content)) != info.Size() {
		return Document{}, fmt.Errorf("attachment changed while being read: %s", sourceAbs)
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	manifest, _ := ReadManifest(scanPath, taskID)
	for _, doc := range manifest.Docs {
		if doc.SHA256 == sha {
			return doc, nil
		}
	}
	docID := uniqueDocID(sha[:16], manifest.Docs)
	storedName := uniqueStoredName(sanitizeFileName(filepath.Base(sourceAbs)), sha, manifest.Docs)
	tmp, err := os.CreateTemp(filesDir, ".attach-*")
	if err != nil {
		return Document{}, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return Document{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Document{}, err
	}
	if err := os.Rename(tmpName, filepath.Join(filesDir, storedName)); err != nil {
		_ = os.Remove(tmpName)
		return Document{}, err
	}
	doc := Document{
		DocID:           docID,
		OriginalName:    filepath.Base(sourceAbs),
		StoredName:      storedName,
		SourcePath:      sourceAbs,
		SHA256:          sha,
		SizeBytes:       info.Size(),
		MIMEOrExtension: extension(sourceAbs),
		AttachedAt:      time.Now().UTC().Format(time.RFC3339),
		AttachedBy:      empty(attachedBy, "p2r"),
		Notes:           note,
	}
	classify(&doc, limits)
	manifest.ManifestVersion = ManifestVersion
	manifest.TaskID = taskID
	manifest.Docs = append(manifest.Docs, doc)
	sort.SliceStable(manifest.Docs, func(i, j int) bool {
		return manifest.Docs[i].AttachedAt < manifest.Docs[j].AttachedAt
	})
	if err := writeManifest(scanPath, taskID, manifest); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func ImportDropbox(scanPath, taskID string, limits config.DocsConfig, attachedBy string) ([]Document, error) {
	dir := filepath.Join(filepath.Clean(scanPath), "task-docs", taskID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var docs []Document
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		doc, err := Attach(scanPath, taskID, filepath.Join(dir, entry.Name()), "imported from task dropbox", attachedBy, limits)
		if err != nil {
			continue
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

func ReadManifest(scanPath, taskID string) (Manifest, error) {
	path := ManifestPath(scanPath, taskID)
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{ManifestVersion: ManifestVersion, TaskID: taskID}, nil
		}
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.ManifestVersion == 0 {
		manifest.ManifestVersion = ManifestVersion
	}
	if manifest.TaskID == "" {
		manifest.TaskID = taskID
	}
	return manifest, nil
}

func Count(scanPath, taskID string) int {
	manifest, err := ReadManifest(scanPath, taskID)
	if err != nil {
		return 0
	}
	return len(manifest.Docs)
}

func BuildContext(scanPath, taskID string, limits config.DocsConfig) (ContextResult, error) {
	return BuildContextFiltered(scanPath, taskID, limits, nil)
}

func BuildContextFiltered(scanPath, taskID string, limits config.DocsConfig, includeText func(Document) bool) (ContextResult, error) {
	manifest, err := ReadManifest(scanPath, taskID)
	if err != nil {
		return ContextResult{}, err
	}
	var builder strings.Builder
	var used []Document
	var total int64
	for _, doc := range manifest.Docs {
		used = append(used, doc)
		if includeText != nil && !includeText(doc) {
			builder.WriteString(fmt.Sprintf("\nAttached doc %s (%s) not embedded: excluded for this stage\n", doc.OriginalName, doc.StoredName))
			continue
		}
		if !doc.TextIncluded {
			builder.WriteString(fmt.Sprintf("\nAttached doc %s (%s) not embedded: %s\n", doc.OriginalName, doc.StoredName, doc.SkipReason))
			continue
		}
		if limits.StageInlineMaxBytes > 0 && total+doc.SizeBytes > limits.StageInlineMaxBytes {
			builder.WriteString(fmt.Sprintf("\nAttached doc %s (%s) not embedded: stage inline limit exceeded\n", doc.OriginalName, doc.StoredName))
			continue
		}
		path := filepath.Join(StoreDir(scanPath, taskID), "files", doc.StoredName)
		content, err := os.ReadFile(path)
		if err != nil {
			builder.WriteString(fmt.Sprintf("\nAttached doc %s (%s) not embedded: %s\n", doc.OriginalName, doc.StoredName, err.Error()))
			continue
		}
		total += int64(len(content))
		builder.WriteString(fmt.Sprintf("\n--- BEGIN UNTRUSTED ATTACHED DOC: %s (%s) ---\n%s\n--- END UNTRUSTED ATTACHED DOC ---\n", path, doc.OriginalName, string(content)))
	}
	return ContextResult{Text: builder.String(), Docs: used}, nil
}

func StoreDir(scanPath, taskID string) string {
	return filepath.Join(filepath.Clean(scanPath), ".qa-control", "task-docs", taskID)
}

func ManifestPath(scanPath, taskID string) string {
	return filepath.Join(StoreDir(scanPath, taskID), "manifest.json")
}

func writeManifest(scanPath, taskID string, manifest Manifest) error {
	dir := StoreDir(scanPath, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ManifestPath(scanPath, taskID), append(content, '\n'), 0o644)
}

func classify(doc *Document, limits config.DocsConfig) {
	if !isTextLike(doc.MIMEOrExtension) {
		doc.TextIncluded = false
		doc.SkipReason = "unsupported binary document for MVP"
		return
	}
	if limits.InlineTextLimitBytes > 0 && doc.SizeBytes > limits.InlineTextLimitBytes {
		doc.TextIncluded = false
		doc.SkipReason = fmt.Sprintf("exceeds inline text limit (%d bytes)", limits.InlineTextLimitBytes)
		return
	}
	doc.TextIncluded = true
	doc.IncludedInStages = []string{"F"}
}

func isTextLike(ext string) bool {
	switch strings.ToLower(ext) {
	case ".md", ".txt", ".json", ".yaml", ".yml", ".csv", ".log":
		return true
	default:
		return false
	}
}

func extension(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "unknown"
	}
	return ext
}

func uniqueDocID(base string, docs []Document) string {
	existing := map[string]bool{}
	for _, doc := range docs {
		existing[doc.DocID] = true
	}
	if !existing[base] {
		return base
	}
	for i := 2; ; i++ {
		id := fmt.Sprintf("%s-%d", base, i)
		if !existing[id] {
			return id
		}
	}
}

func uniqueStoredName(name, sha string, docs []Document) string {
	if name == "" || name == "." {
		name = sha[:16] + ".txt"
	}
	existing := map[string]bool{}
	for _, doc := range docs {
		existing[doc.StoredName] = true
	}
	if !existing[name] {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	candidate := base + "-" + sha[:8] + ext
	if !existing[candidate] {
		return candidate
	}
	for i := 2; ; i++ {
		candidate = fmt.Sprintf("%s-%s-%d%s", base, sha[:8], i, ext)
		if !existing[candidate] {
			return candidate
		}
	}
}

func sanitizeFileName(name string) string {
	name = filepath.Base(name)
	name = strings.ReplaceAll(name, string(filepath.Separator), "_")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "attachment"
	}
	return name
}

func inside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)))
}

func empty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
