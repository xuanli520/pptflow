package image2

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xuanli520/pptflow/internal/workflow"
)

const (
	defaultBaseURL = "https://new-api.metalics.cn/v1"
	defaultModel   = "gpt-image-2"
	defaultSize    = "1536x1024"
	defaultQuality = "high"
)

type Runtime struct {
	client  *http.Client
	baseURL string
	apiKey  string
	model   string
}

func NewFromEnv() Runtime {
	return Runtime{
		client:  http.DefaultClient,
		baseURL: envFirst("PPTFLOW_IMAGE_BASE_URL", defaultBaseURL),
		apiKey:  envFirst("PPTFLOW_IMAGE_API_KEY", "OPENAI_API_KEY"),
		model:   envFirst("PPTFLOW_IMAGE_MODEL", defaultModel),
	}
}

func New(baseURL, apiKey, model string, client *http.Client) Runtime {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	return Runtime{client: client, baseURL: strings.TrimRight(baseURL, "/"), apiKey: strings.TrimSpace(apiKey), model: strings.TrimSpace(model)}
}

func (r Runtime) Configured() bool {
	return strings.TrimSpace(r.apiKey) != ""
}

func (r Runtime) Generate(ctx context.Context, req workflow.ImageRequest) (workflow.ImageResult, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return workflow.ImageResult{}, fmt.Errorf("image prompt is required")
	}
	if strings.TrimSpace(req.OutputPath) == "" {
		return workflow.ImageResult{}, fmt.Errorf("image output path is required")
	}
	if !r.Configured() {
		return workflow.ImageResult{}, fmt.Errorf("image API key is not configured")
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	size := strings.TrimSpace(req.Size)
	if size == "" {
		size = defaultSize
	}
	quality := strings.TrimSpace(req.Quality)
	if quality == "" {
		quality = defaultQuality
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = r.model
	}
	body := map[string]any{
		"model":   model,
		"prompt":  req.Prompt,
		"size":    size,
		"quality": quality,
	}
	if len(req.SourceImages) > 0 {
		sources, err := sourceImagePayload(req.SourceImages)
		if err != nil {
			return workflow.ImageResult{}, err
		}
		body["source_images"] = sources
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return workflow.ImageResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.baseURL, "/")+"/images/generations", bytes.NewReader(payload))
	if err != nil {
		return workflow.ImageResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+r.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(httpReq)
	if err != nil {
		return workflow.ImageResult{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return workflow.ImageResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return workflow.ImageResult{}, fmt.Errorf("image API returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	image, err := decodeImagePayload(ctx, r.client, data)
	if err != nil {
		return workflow.ImageResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(req.OutputPath), 0o755); err != nil {
		return workflow.ImageResult{}, err
	}
	if err := os.WriteFile(req.OutputPath, image, 0o644); err != nil {
		return workflow.ImageResult{}, err
	}
	return workflow.ImageResult{Path: req.OutputPath, Model: model, Size: size, Quality: quality, MIME: "image/png"}, nil
}

func sourceImagePayload(sources []workflow.ImageSource) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		item := map[string]any{}
		if path := strings.TrimSpace(source.Path); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read source image %s: %w", path, err)
			}
			item["image"] = "data:" + imageMIME(path) + ";base64," + base64.StdEncoding.EncodeToString(data)
			item["path"] = path
		} else if url := strings.TrimSpace(source.URL); url != "" {
			item["url"] = url
		} else {
			continue
		}
		if role := strings.TrimSpace(source.Role); role != "" {
			item["role"] = role
		}
		if detail := strings.TrimSpace(source.Detail); detail != "" {
			item["detail"] = detail
		}
		result = append(result, item)
	}
	return result, nil
}

func imageMIME(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func decodeImagePayload(ctx context.Context, client *http.Client, data []byte) ([]byte, error) {
	var payload struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
			URL     string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if len(payload.Data) == 0 {
		return nil, fmt.Errorf("image API response contains no data")
	}
	if payload.Data[0].B64JSON != "" {
		return base64.StdEncoding.DecodeString(payload.Data[0].B64JSON)
	}
	if payload.Data[0].URL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, payload.Data[0].URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("image download returned %s", resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return nil, fmt.Errorf("image API response missing b64_json or url")
}

func envFirst(primary, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(primary)); value != "" {
		return value
	}
	if strings.HasPrefix(fallback, "PPTFLOW_") || strings.HasPrefix(fallback, "OPENAI_") {
		return strings.TrimSpace(os.Getenv(fallback))
	}
	return fallback
}
