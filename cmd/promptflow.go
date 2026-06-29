package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/xuanli520/pptflow/internal/app"
)

func newPromptFlowCommand() *cobra.Command {
	var (
		prompt              string
		model               string
		imageModel          string
		imageSize           string
		imageQuality        string
		artifactRoot        string
		workspaceRoot       string
		imageTimeoutSeconds int
		codexTimeoutSeconds int
		requireImages       bool
	)

	cmd := &cobra.Command{
		Use:   "promptflow",
		Short: "Generate a presentation from a natural language prompt",
		Long: `PromptFlow is the PPTflow V2 pipeline that generates beautiful, editable presentations.

Pipeline:
  1. Codex optimizes your prompt and extracts structured requirements
  2. Codex generates a slide-by-slide outline and visual style spec
  3. Image2 creates a style reference image (cover slide)
  4. Image2 generates each slide as a high-quality image (NotebookLM-style)
  5. Codex analyzes each slide image to extract the visual layout
  6. Image2 extracts individual resources (images, shapes) for editability
  7. Codex assembles the final editable PPTX

Requires:
  - Codex CLI on PATH (with app-server capability)
  - Image2 API configured (PPTFLOW_IMAGE_API_KEY env var) if --require-images is set`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if prompt == "" {
				return fmt.Errorf("--prompt is required")
			}
			result, err := app.RunPromptFlow(cmd.Context(), app.PromptFlowOptions{
				Prompt:              prompt,
				Model:               model,
				ImageModel:          imageModel,
				ImageSize:           imageSize,
				ImageQuality:        imageQuality,
				ArtifactRoot:        artifactRoot,
				WorkspaceRoot:       workspaceRoot,
				ImageTimeoutSeconds: imageTimeoutSeconds,
				CodexTimeoutSeconds: codexTimeoutSeconds,
				RequireImages:       requireImages,
			})
			if err != nil {
				return err
			}
			fmt.Printf("Workflow %s completed: %s\n", result.WorkflowID, result.Status)
			fmt.Printf("Artifacts: %s\n", result.ArtifactRoot)
			fmt.Printf("Duration: %dms\n", result.DurationMS)
			for _, node := range result.Nodes {
				status := string(node.Status)
				if node.Error != "" {
					status = fmt.Sprintf("%s (%s)", status, node.Error)
				}
				fmt.Printf("  [%s] %s: %s (%dms)\n", status, node.NodeID, node.Name, node.DurationMS)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&prompt, "prompt", "p", "", "Natural language prompt describing the presentation (required)")
	cmd.Flags().StringVar(&model, "model", "", "Codex model (default: gpt-5.5)")
	cmd.Flags().StringVar(&imageModel, "image-model", "", "Image2 model (default: gpt-image-2)")
	cmd.Flags().StringVar(&imageSize, "image-size", "1536x1024", "Image2 output size")
	cmd.Flags().StringVar(&imageQuality, "image-quality", "high", "Image2 output quality")
	cmd.Flags().StringVar(&artifactRoot, "artifact-root", "artifacts", "Artifact storage root directory")
	cmd.Flags().StringVar(&workspaceRoot, "workspace-root", "workspace", "Codex workspace root directory")
	cmd.Flags().IntVar(&imageTimeoutSeconds, "image-timeout", 180, "Timeout per image generation (seconds)")
	cmd.Flags().IntVar(&codexTimeoutSeconds, "codex-timeout", 300, "Timeout per Codex turn (seconds)")
	cmd.Flags().BoolVar(&requireImages, "require-images", false, "Fail if Image2 API is not configured")

	_ = cmd.MarkFlagRequired("prompt")
	return cmd
}
