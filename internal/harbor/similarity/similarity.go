package similarity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/purplevoid/harbor-factory/internal/harbor/domain"
	"github.com/purplevoid/harbor-factory/internal/harbor/harborrun"
	"github.com/purplevoid/harbor-factory/internal/harbor/repourl"
	"github.com/purplevoid/harbor-factory/internal/harbor/sanitize"
)

const maxLocalSourceFileSize = 1 << 20

type localSourceFile struct {
	Path    string
	RelPath string
	Size    int64
	Digest  string
	Content []byte
}

type Options struct {
	TaskDir           string
	RepoURL           string
	TestsAnalysisPath string
	HistoryDirs       []string
	TB3Dirs           []string
	EnableGitHub      bool
	GitHubToken       string
	GitHubBaseURL     string
	HTTPClient        *http.Client
	Threshold         float64
	StrictSources     bool
	WriteReport       string
}

func Run(ctx context.Context, opts Options) (domain.SimilarityReport, error) {
	threshold := opts.Threshold
	if threshold <= 0 {
		threshold = 0.42
	}
	report := domain.SimilarityReport{
		SchemaVersion: "harbor.similarity_report.v1",
		TaskDir:       strings.TrimSpace(opts.TaskDir),
		RepoURL:       repourl.StripCredentials(strings.TrimSpace(opts.RepoURL)),
		Threshold:     threshold,
		OverallPass:   true,
		CreatedAt:     time.Now().UTC(),
	}
	if err := repourl.RejectCredentials(opts.RepoURL); err != nil {
		report.OverallPass = false
		report.Issues = append(report.Issues, "repo URL must not include credentials, query, or fragment")
		opts.RepoURL = report.RepoURL
	}
	taskDigest, err := harborrun.ComputeTaskDigest(opts.TaskDir)
	if err != nil {
		report.OverallPass = false
		report.Issues = append(report.Issues, "task digest cannot be computed for similarity check: "+err.Error())
		return finish(report, opts.WriteReport)
	}
	report.TaskDigest = taskDigest
	sourceText, err := taskText(opts.TaskDir, opts.TestsAnalysisPath)
	if err != nil {
		report.OverallPass = false
		report.Issues = append(report.Issues, err.Error())
		return finish(report, opts.WriteReport)
	}
	for _, dir := range opts.HistoryDirs {
		if strings.TrimSpace(dir) != "" {
			report.Sources = append(report.Sources, LocalSourceID("history", dir))
		}
		candidates, evidence, err := scanLocalSource("history", dir, sourceText, threshold)
		if err != nil {
			addSourceWarning(&report, opts.StrictSources, err.Error())
			continue
		}
		report.ScannedFileCount += evidence.ScannedFileCount
		if evidence.ScannedFileCount == 0 {
			addSourceWarning(&report, opts.StrictSources, "history similarity directory has no text-like files: "+strings.TrimSpace(dir))
		} else {
			report.SuccessfulSources = append(report.SuccessfulSources, evidence.Source)
			report.SourceEvidence = append(report.SourceEvidence, evidence)
		}
		report.Candidates = append(report.Candidates, candidates...)
	}
	for _, dir := range opts.TB3Dirs {
		if strings.TrimSpace(dir) != "" {
			report.Sources = append(report.Sources, LocalSourceID("tb3", dir))
		}
		candidates, evidence, err := scanLocalSource("tb3", dir, sourceText, threshold)
		if err != nil {
			addSourceWarning(&report, opts.StrictSources, err.Error())
			continue
		}
		report.ScannedFileCount += evidence.ScannedFileCount
		if evidence.ScannedFileCount == 0 {
			addSourceWarning(&report, opts.StrictSources, "tb3 similarity directory has no text-like files: "+strings.TrimSpace(dir))
		} else {
			report.SuccessfulSources = append(report.SuccessfulSources, evidence.Source)
			report.SourceEvidence = append(report.SourceEvidence, evidence)
		}
		report.Candidates = append(report.Candidates, candidates...)
	}
	if opts.EnableGitHub {
		report.Sources = append(report.Sources, "github")
		candidates, warnings, evidence, success := searchGitHub(ctx, opts, sourceText, threshold)
		report.Candidates = append(report.Candidates, candidates...)
		if success {
			report.SuccessfulSources = append(report.SuccessfulSources, "github")
			report.SourceEvidence = append(report.SourceEvidence, evidence)
		}
		for _, warning := range warnings {
			addSourceWarning(&report, opts.StrictSources, warning)
		}
	}
	if len(report.Sources) == 0 {
		report.Warnings = append(report.Warnings, "no similarity sources configured")
		if opts.StrictSources {
			report.OverallPass = false
			report.Issues = append(report.Issues, "at least one similarity source is required")
		}
	}
	if len(report.Sources) > 0 && len(report.SuccessfulSources) == 0 {
		report.OverallPass = false
		report.Issues = append(report.Issues, "at least one similarity source must be scanned successfully")
	}
	sort.Slice(report.Candidates, func(i, j int) bool {
		return report.Candidates[i].Score > report.Candidates[j].Score
	})
	for _, candidate := range report.Candidates {
		if candidate.Score > report.MaxScore {
			report.MaxScore = candidate.Score
		}
	}
	if report.MaxScore >= threshold {
		report.OverallPass = false
		report.Issues = append(report.Issues, fmt.Sprintf("max similarity %.3f exceeds threshold %.3f", report.MaxScore, threshold))
	}
	return finish(report, opts.WriteReport)
}

func addSourceWarning(report *domain.SimilarityReport, strict bool, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	report.Warnings = append(report.Warnings, message)
	if strict {
		report.OverallPass = false
		report.Issues = append(report.Issues, message)
	}
}

func taskText(taskDir, testsAnalysisPath string) (string, error) {
	taskDir = strings.TrimSpace(taskDir)
	if taskDir == "" {
		return "", fmt.Errorf("task directory is required for similarity check")
	}
	var parts []string
	for _, rel := range []string{"instruction.md", "task.toml"} {
		data, err := os.ReadFile(filepath.Join(taskDir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("cannot read %s for similarity check: %w", rel, err)
		}
		parts = append(parts, string(data))
	}
	for _, rel := range taskReferenceFiles(taskDir) {
		data, _ := os.ReadFile(filepath.Join(taskDir, filepath.FromSlash(rel)))
		if len(data) > 0 {
			parts = append(parts, string(data))
		}
	}
	if strings.TrimSpace(testsAnalysisPath) != "" {
		data, _ := os.ReadFile(testsAnalysisPath)
		if len(data) > 0 {
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n"), nil
}

func taskReferenceFiles(taskDir string) []string {
	seen := map[string]bool{
		"instruction.md": true,
		"task.toml":      true,
	}
	var files []string
	add := func(rel string) {
		rel = filepath.ToSlash(filepath.Clean(rel))
		if rel == "." || seen[rel] {
			return
		}
		seen[rel] = true
		files = append(files, rel)
	}
	if info, err := os.Stat(filepath.Join(taskDir, "tests_analysis.md")); err == nil && info.Mode().IsRegular() && info.Size() <= maxLocalSourceFileSize {
		add("tests_analysis.md")
	}
	for _, root := range []string{"tests", "solution", "environment"} {
		base := filepath.Join(taskDir, root)
		_ = filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.IsDir() {
				if path != base && strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 || !textLike(path) {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > maxLocalSourceFileSize {
				return nil
			}
			rel, err := filepath.Rel(taskDir, path)
			if err != nil {
				return nil
			}
			add(rel)
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func scanLocalSource(source, dir, reference string, threshold float64) ([]domain.SimilarityCandidate, domain.SimilaritySourceEvidence, error) {
	files, evidence, err := collectLocalSourceFiles(source, dir)
	if err != nil {
		return nil, domain.SimilaritySourceEvidence{}, err
	}
	refTokens := tokenSet(reference)
	var candidates []domain.SimilarityCandidate
	for _, file := range files {
		score, terms := similarity(refTokens, tokenSet(string(file.Content)))
		if score >= threshold {
			candidates = append(candidates, domain.SimilarityCandidate{
				Source:       source,
				Title:        filepath.Base(file.Path),
				Path:         file.Path,
				Score:        score,
				MatchedTerms: terms,
			})
		}
	}
	return candidates, evidence, nil
}

func BuildLocalSourceEvidence(source, dir string) (domain.SimilaritySourceEvidence, error) {
	_, evidence, err := collectLocalSourceFiles(source, dir)
	return evidence, err
}

func LocalSourceID(source, dir string) string {
	source = strings.TrimSpace(source)
	path := canonicalLocalSourcePath(dir)
	if path == "" {
		path = strings.TrimSpace(dir)
	}
	return source + ":" + path
}

func collectLocalSourceFiles(source, dir string) ([]localSourceFile, domain.SimilaritySourceEvidence, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, domain.SimilaritySourceEvidence{}, nil
	}
	root := canonicalLocalSourcePath(dir)
	if root == "" {
		root = filepath.Clean(dir)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, domain.SimilaritySourceEvidence{}, fmt.Errorf("%s similarity directory is not readable: %s", source, dir)
	}
	var files []localSourceFile
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !textLike(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxLocalSourceFileSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, localSourceFile{
			Path:    path,
			RelPath: filepath.ToSlash(rel),
			Size:    info.Size(),
			Digest:  hex.EncodeToString(sum[:]),
			Content: data,
		})
		return nil
	})
	if err != nil {
		return nil, domain.SimilaritySourceEvidence{}, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].RelPath < files[j].RelPath
	})
	evidence := domain.SimilaritySourceEvidence{
		Source:           source + ":" + root,
		Kind:             source,
		Path:             root,
		ScannedFileCount: len(files),
		SourceDigest:     digestLocalSource(source, files),
	}
	return files, evidence, nil
}

func canonicalLocalSourcePath(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	cleaned := filepath.Clean(dir)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
		abs = evaluated
	}
	return filepath.Clean(abs)
}

func digestLocalSource(source string, files []localSourceFile) string {
	hash := sha256.New()
	fmt.Fprintf(hash, "harbor.similarity_source.v1\nkind=%s\n", source)
	for _, file := range files {
		fmt.Fprintf(hash, "%s\x00%d\x00%s\n", file.RelPath, file.Size, file.Digest)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func searchGitHub(ctx context.Context, opts Options, reference string, threshold float64) ([]domain.SimilarityCandidate, []string, domain.SimilaritySourceEvidence, bool) {
	evidence := domain.SimilaritySourceEvidence{
		Source: "github",
		Kind:   "github",
	}
	owner, repo, ok := repourl.GitHubOwnerRepo(opts.RepoURL)
	if !ok {
		return nil, []string{"github similarity skipped: repo URL is not a GitHub repo"}, evidence, false
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	baseURL := strings.TrimRight(opts.GitHubBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	queryTerms := strings.Join(topTerms(reference, 8), " ")
	if queryTerms == "" {
		return nil, []string{"github similarity skipped: no useful query terms"}, evidence, false
	}
	var candidates []domain.SimilarityCandidate
	var warnings []string
	success := false
	for _, kind := range []string{"issue", "pr"} {
		q := fmt.Sprintf("repo:%s/%s is:%s %s", owner, repo, kind, queryTerms)
		endpoint := baseURL + "/search/issues?q=" + url.QueryEscape(q)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			warnings = append(warnings, "github search request failed: "+err.Error())
			continue
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if strings.TrimSpace(opts.GitHubToken) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.GitHubToken))
		}
		resp, err := client.Do(req)
		if err != nil {
			warnings = append(warnings, "github search failed: "+err.Error())
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		evidence.HTTPStatuses = append(evidence.HTTPStatuses, resp.StatusCode)
		if resp.StatusCode >= 300 {
			warnings = append(warnings, fmt.Sprintf("github search %s returned %s", kind, resp.Status))
			continue
		}
		var decoded struct {
			Items []struct {
				HTMLURL string `json:"html_url"`
				Title   string `json:"title"`
				Body    string `json:"body"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			warnings = append(warnings, "github search decode failed: "+err.Error())
			continue
		}
		success = true
		evidence.QueryCount++
		evidence.ResultCount += len(decoded.Items)
		refTokens := tokenSet(reference)
		for _, item := range decoded.Items {
			score, terms := similarity(refTokens, tokenSet(item.Title+"\n"+item.Body))
			if score >= threshold {
				candidates = append(candidates, domain.SimilarityCandidate{
					Source:       "github_issue_pr",
					Title:        item.Title,
					URL:          item.HTMLURL,
					Score:        score,
					MatchedTerms: terms,
				})
			}
		}
	}
	return candidates, warnings, evidence, success
}

var tokenPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]{2,}`)

func tokenSet(text string) map[string]bool {
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
		"from": true, "into": true, "task": true, "test": true, "tests": true, "should": true,
		"must": true, "will": true, "you": true, "are": true, "not": true, "fix": true,
	}
	tokens := map[string]bool{}
	for _, token := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		if !stop[token] && len(token) <= 40 {
			tokens[token] = true
		}
	}
	return tokens
}

func similarity(a, b map[string]bool) (float64, []string) {
	if len(a) == 0 || len(b) == 0 {
		return 0, nil
	}
	intersection := 0
	var terms []string
	for token := range a {
		if b[token] {
			intersection++
			terms = append(terms, token)
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0, nil
	}
	sort.Strings(terms)
	if len(terms) > 12 {
		terms = terms[:12]
	}
	score := float64(intersection) / float64(union)
	return math.Round(score*10000) / 10000, terms
}

func topTerms(text string, limit int) []string {
	counts := map[string]int{}
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
		"from": true, "into": true, "task": true, "test": true, "tests": true, "should": true,
		"must": true, "will": true, "you": true, "are": true, "not": true, "fix": true,
	}
	for _, token := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		if stop[token] || len(token) > 40 {
			continue
		}
		counts[token]++
	}
	type kv struct {
		key   string
		count int
	}
	var pairs []kv
	for key, count := range counts {
		pairs = append(pairs, kv{key: key, count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count == pairs[j].count {
			return pairs[i].key < pairs[j].key
		}
		return pairs[i].count > pairs[j].count
	})
	var terms []string
	for _, pair := range pairs {
		terms = append(terms, pair.key)
		if len(terms) >= limit {
			break
		}
	}
	return terms
}

func textLike(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".toml", ".json", ".yaml", ".yml", ".sh":
		return true
	default:
		return false
	}
}

func readSmallFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxLocalSourceFileSize {
		return nil, err
	}
	return os.ReadFile(path)
}

func finish(report domain.SimilarityReport, writePath string) (domain.SimilarityReport, error) {
	report = sanitize.SimilarityReport(report)
	if strings.TrimSpace(writePath) == "" {
		return report, nil
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o755); err != nil {
		return report, err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return report, err
	}
	return report, os.WriteFile(writePath, append(data, '\n'), 0o644)
}
