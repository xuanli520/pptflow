package cmd

import (
	"archive/tar"
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/internal/app"
	"github.com/purplevoid/harbor-factory/internal/harbor/store"
	"github.com/purplevoid/harbor-factory/internal/harbor/workflowadapter"
	"github.com/purplevoid/harbor-factory/internal/workflowruntime"
	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func createCommandStandardAuthoringRun(t *testing.T, ctx context.Context, root string, services *app.LifecycleServices, slug, title, actor string) (store.TaskV2, store.AuthoringSession, store.WorkflowRun, *workflowruntime.ArtifactObjectStore) {
	t.Helper()
	objects, err := workflowruntime.NewArtifactObjectStore(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	sourceObject, err := objects.PutBytes(ctx, commandStandardSourceSnapshot(t))
	if err != nil {
		t.Fatalf("store source snapshot fixture: %v", err)
	}
	reference := workflowadapter.StandardAuthoringCurrentTemplateReference()
	repositoryURL := "https://github.com/purplevoid/" + slug + ".git"
	commitSHA := strings.Repeat("1", 40)
	source, err := services.Store().CreateAuthoringSource(ctx, store.CreateAuthoringSourceRequest{
		RepositoryURL: repositoryURL, CommitSHA: commitSHA,
		SnapshotArtifactRef:   string(sourceObject.Digest),
		SnapshotContentDigest: string(sourceObject.Digest),
		SnapshotSchemaVersion: app.StandardAuthoringSourceSnapshotSchemaVersion,
		IdempotencyKey:        "command-standard-source:" + slug,
		Actor:                 actor,
		Reason:                "freeze Standard source fixture",
	})
	if err != nil {
		t.Fatalf("create Standard source fixture: %v", err)
	}
	task, err := services.Store().CreateTaskV2(ctx, store.CreateTaskV2Request{
		Slug: slug, Title: title, SourceRepo: source.RepositoryURL, SourceCommit: source.CommitSHA,
		Actor: actor, Reason: "reserve Standard draft task fixture",
	})
	if err != nil {
		t.Fatalf("create Standard draft task fixture: %v", err)
	}
	session, err := services.Store().CreateAuthoringSession(ctx, store.CreateAuthoringSessionRequest{
		SourceID: source.ID, TargetTaskID: task.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		SessionManifestJSON: `{"mode":"standard","fixture":true}`,
		IdempotencyKey:      "command-standard-session:" + slug,
		Actor:               actor,
		Reason:              "freeze Standard session fixture",
	})
	if err != nil {
		t.Fatalf("create Standard session fixture: %v", err)
	}
	run, err := services.Store().CreateAuthoringWorkflowRun(ctx, store.CreateAuthoringWorkflowRunRequest{
		AuthoringSessionID: session.ID,
		WorkflowTemplateID: reference.ID, WorkflowTemplateVersion: reference.Version,
		ResolvedProfileHash: string(workflowkit.SHA256Fingerprint([]byte("command-standard-profile:" + slug))),
		DefinitionHash:      string(workflowkit.SHA256Fingerprint([]byte("command-standard-definition:" + slug))),
		RunManifestJSON:     `{}`,
		Trigger:             "command-standard-fixture",
		Actor:               actor,
		Reason:              "start Standard fixture Run",
	})
	if err != nil {
		t.Fatalf("create Standard authoring Run fixture: %v", err)
	}
	return task, session, run, objects
}

func commandStandardSourceSnapshot(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	entries := []struct {
		name    string
		mode    int64
		content []byte
		dir     bool
	}{
		{name: "source/", mode: 0o755, dir: true},
		{name: "source/README.md", mode: 0o644, content: []byte("fixture source tree\n")},
	}
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode}
		if entry.dir {
			header.Typeflag = tar.TypeDir
		} else {
			header.Typeflag = tar.TypeReg
			header.Size = int64(len(entry.content))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if !entry.dir {
			if _, err := writer.Write(entry.content); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
