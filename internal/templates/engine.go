package templates

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
)

//go:embed phase1/*.md phase2/*.md
var templateFS embed.FS

var versionPattern = regexp.MustCompile(`\{\{/\*\s*template-version:\s*([^*]+?)\s*\*/\}\}`)

// Metadata identifies the exact embedded prompt used for an agent request.
// Digest covers the original template bytes, including its version marker.
type Metadata struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type compiledTemplate struct {
	template *template.Template
	metadata Metadata
}

// Engine renders the versioned prompt templates embedded in the binary.
type Engine struct {
	templates map[string]compiledTemplate
}

// New parses every embedded prompt eagerly so a malformed template fails at
// application construction rather than halfway through a workflow.
func New() (*Engine, error) {
	files, err := fs.Glob(templateFS, "phase*/*.md")
	if err != nil {
		return nil, fmt.Errorf("list prompt templates: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no prompt templates embedded")
	}
	sort.Strings(files)
	engine := &Engine{templates: make(map[string]compiledTemplate, len(files))}
	for _, filename := range files {
		raw, err := templateFS.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("read prompt template %s: %w", filename, err)
		}
		name := strings.TrimSuffix(path.Clean(filename), path.Ext(filename))
		versionMatch := versionPattern.FindSubmatch(raw)
		if len(versionMatch) != 2 || strings.TrimSpace(string(versionMatch[1])) == "" {
			return nil, fmt.Errorf("prompt template %s is missing template-version", filename)
		}
		parsed, err := template.New(name).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("parse prompt template %s: %w", filename, err)
		}
		digest := sha256.Sum256(raw)
		engine.templates[name] = compiledTemplate{
			template: parsed,
			metadata: Metadata{
				Name:    name,
				Version: strings.TrimSpace(string(versionMatch[1])),
				Digest:  fmt.Sprintf("sha256:%x", digest),
			},
		}
	}
	return engine, nil
}

// Render executes a named template. Names are relative paths without the .md
// extension, for example "phase1/repo_analyze".
func (e *Engine) Render(name string, data any) (string, error) {
	name = normalizeName(name)
	if e == nil {
		return "", fmt.Errorf("prompt template engine is nil")
	}
	entry, ok := e.templates[name]
	if !ok {
		return "", fmt.Errorf("prompt template %q not found", name)
	}
	var output bytes.Buffer
	if err := entry.template.Execute(&output, data); err != nil {
		return "", fmt.Errorf("render prompt template %s: %w", name, err)
	}
	return output.String(), nil
}

// Metadata returns version and content digest information for a template.
func (e *Engine) Metadata(name string) (Metadata, bool) {
	if e == nil {
		return Metadata{}, false
	}
	entry, ok := e.templates[normalizeName(name)]
	return entry.metadata, ok
}

// MetadataList returns stable, name-sorted metadata for all templates.
func (e *Engine) MetadataList() []Metadata {
	if e == nil {
		return nil
	}
	result := make([]Metadata, 0, len(e.templates))
	for _, entry := range e.templates {
		result = append(result, entry.metadata)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func normalizeName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = strings.TrimSuffix(name, ".md")
	return strings.TrimPrefix(path.Clean(name), "./")
}

var (
	defaultOnce   sync.Once
	defaultEngine *Engine
	defaultErr    error
)

// Default returns the process-wide immutable embedded template engine.
func Default() (*Engine, error) {
	defaultOnce.Do(func() {
		defaultEngine, defaultErr = New()
	})
	return defaultEngine, defaultErr
}
