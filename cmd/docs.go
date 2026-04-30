package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xuanli520/p2r_tui/internal/taskdocs"
)

func newAttachCommand() *cobra.Command {
	var filePath string
	var note string
	command := &cobra.Command{
		Use:          "attach <task-id>",
		Short:        "Attach a supplemental QA document to a task",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if filePath == "" {
				return fmt.Errorf("--file is required")
			}
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			store, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer store.Close()
			if _, err := store.GetProject(cmd.Context(), args[0]); err != nil {
				return err
			}
			doc, err := taskdocs.Attach(cfg.ScanPath, args[0], filePath, note, "operator", cfg.Docs)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "attached %s as %s (%s)\n", doc.OriginalName, doc.DocID, doc.StoredName)
			return nil
		},
	}
	command.Flags().StringVar(&filePath, "file", "", "file path to attach")
	command.Flags().StringVar(&note, "note", "", "operator note")
	return command
}

func newDocsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:          "docs <task-id>",
		Short:        "List supplemental QA documents for a task",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			manifest, err := taskdocs.ReadManifest(cfg.ScanPath, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "task: %s\ndocs: %d\nmanifest: %s\n", args[0], len(manifest.Docs), taskdocs.ManifestPath(cfg.ScanPath, args[0]))
			for _, doc := range manifest.Docs {
				status := "inline"
				if !doc.TextIncluded {
					status = "listed: " + doc.SkipReason
				}
				fmt.Fprintf(out, "- %s  %s  %s  %d bytes  %s\n", doc.DocID, doc.OriginalName, doc.StoredName, doc.SizeBytes, status)
			}
			return nil
		},
	}
	command.AddCommand(newDocsImportDropboxCommand())
	return command
}

func newDocsImportDropboxCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "import-dropbox <task-id>",
		Short:        "Import files from projects-qa/task-docs/<task-id>",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig("")
			if err != nil {
				return err
			}
			docs, err := taskdocs.ImportDropbox(cfg.ScanPath, args[0], cfg.Docs, "operator-dropbox")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d docs for %s\n", len(docs), args[0])
			return nil
		},
	}
}
