package workflowadapter

import (
	"bytes"
	"strings"
	"testing"

	"github.com/purplevoid/harbor-factory/pkg/workflowkit"
)

func TestAuthoringContractCanonicalRoundTripAndDigest(t *testing.T) {
	contract, err := NewAuthoringContract(
		AuthoringContractTask{
			ID: "018f0a73-3b49-7000-8000-000000000010", Slug: "bounded-task", Title: "Bounded task",
			CodeLang: "rust", TaskType: "feature", Application: "backend",
		},
		AuthoringContractSource{
			RepositoryURL: "https://github.com/example/repository.git", CommitSHA: strings.Repeat("a", 40),
			SnapshotDigest: string(workflowkit.SHA256Fingerprint([]byte("frozen source"))), CheckoutRoot: "source",
		},
		standardAuthoringPolicyTestImage,
		"Add one bounded behavior.",
		string(workflowkit.SHA256Fingerprint([]byte("profile"))),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := contract.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAuthoringContractJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := parsed.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, reencoded) {
		t.Fatalf("contract canonical round trip = %s, %v", reencoded, err)
	}
	digest, err := contract.ContentDigest()
	if err != nil || digest != workflowkit.SHA256Fingerprint(raw) {
		t.Fatalf("contract digest = %q, %v", digest, err)
	}
}

func TestAuthoringContractRejectsDuplicateFieldsAndInvalidRoot(t *testing.T) {
	duplicate := []byte(`{"format":"harbor.standard-authoring-contract.v2","format":"harbor.standard-authoring-contract.v2"}`)
	if _, err := ParseAuthoringContractJSON(duplicate); err == nil {
		t.Fatal("contract accepted duplicate field")
	}
	contract := AuthoringContract{Format: AuthoringContractFormat, Version: AuthoringContractVersion}
	if err := contract.Validate(); err == nil {
		t.Fatal("contract accepted incomplete root facts")
	}
}
