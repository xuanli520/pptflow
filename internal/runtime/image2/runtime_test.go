package image2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/xuanli520/pptflow/internal/workflow"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

func TestGenerateUsesRequestModelAndSourceImages(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/images/generations" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + onePixelPNG + `"}]}`))
	}))
	defer server.Close()

	sourcePath := filepath.Join(t.TempDir(), "style.png")
	sourceBytes, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, sourceBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(t.TempDir(), "out.png")
	result, err := New(server.URL, "key", "runtime-model", server.Client()).Generate(context.Background(), workflow.ImageRequest{
		Model:      "request-model",
		Prompt:     "make a slide",
		OutputPath: outputPath,
		SourceImages: []workflow.ImageSource{{
			Path:   sourcePath,
			Role:   "style_reference",
			Detail: "high",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "request-model" {
		t.Fatalf("model = %s", result.Model)
	}
	if payload["model"] != "request-model" {
		t.Fatalf("payload model = %v", payload["model"])
	}
	sources, ok := payload["source_images"].([]any)
	if !ok || len(sources) != 1 {
		t.Fatalf("source_images = %#v", payload["source_images"])
	}
	source, ok := sources[0].(map[string]any)
	if !ok || source["role"] != "style_reference" || source["detail"] != "high" || source["image"] == "" {
		t.Fatalf("source = %#v", sources[0])
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatal(err)
	}
}

func TestNewFromEnvUsesDefaultProviderAndOpenAIKeyFallback(t *testing.T) {
	t.Setenv("PPTFLOW_IMAGE_BASE_URL", "")
	t.Setenv("PPTFLOW_IMAGE_API_KEY", "")
	t.Setenv("PPTFLOW_IMAGE_MODEL", "")
	t.Setenv("OPENAI_API_KEY", "openai-key")

	runtime := NewFromEnv()
	if runtime.baseURL != defaultBaseURL {
		t.Fatalf("baseURL = %s", runtime.baseURL)
	}
	if runtime.apiKey != "openai-key" {
		t.Fatalf("apiKey = %s", runtime.apiKey)
	}
	if runtime.model != defaultModel {
		t.Fatalf("model = %s", runtime.model)
	}
}
