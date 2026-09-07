package tools

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestRepositoryOpenAPIContract(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromFile("../api/openapi/restfleet-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := spec.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	schema := spec.Components.Schemas["Repository"].Value
	value := map[string]any{
		"id": "0198f1da-2c57-7d3b-9c92-6e2f05293647", "name": "Archive", "host_id": "0198f1da-2c57-7d3b-9c92-6e2f05293647", "storage_credential_id": "0198f1da-2c57-7d3b-9c92-6e2f05293647",
		"status": "PROVISIONING", "gateway_secret_revision": float64(1), "restic_secret_revision": float64(1),
		"revision": float64(1), "created_at": "2026-09-07T00:00:00Z", "updated_at": "2026-09-07T00:00:00Z",
	}
	if err := schema.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"backend_path", "gateway_username", "gateway_password", "restic_password", "secret_ref", "ciphertext"} {
		value[field] = "forbidden"
		if err := schema.VisitJSON(value); err == nil {
			t.Fatalf("contract permits %s", field)
		}
		delete(value, field)
	}
	value["format_version"] = float64(3)
	if err := schema.VisitJSON(value); err == nil {
		t.Fatal("unknown repository format accepted by contract")
	}
	value["format_version"] = float64(2)
	if err := schema.VisitJSON(value); err != nil {
		t.Fatal(err)
	}
	delete(value, "host_id")
	if err := schema.VisitJSON(value); err == nil {
		t.Fatal("Host ownership optional in contract")
	}
}

func TestStorageProviderContract(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromFile("../api/openapi/restfleet-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	schema := spec.Components.Schemas["StorageCredential"].Value.Properties["provider"].Value
	for _, provider := range []string{"RCLONE_ONEDRIVE", "RCLONE_GDRIVE", "RCLONE_WEBDAV"} {
		if err := schema.VisitJSON(provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := schema.VisitJSON("RCLONE_ARBITRARY"); err == nil {
		t.Fatal("unreviewed backend accepted")
	}
}

func TestInitializeOperationContract(t *testing.T) {
	spec, err := openapi3.NewLoader().LoadFromFile("../api/openapi/restfleet-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := spec.Paths.Find("/api/v1/repositories/{repository_id}/initialize")
	if endpoint == nil || endpoint.Post == nil || endpoint.Post.RequestBody != nil || endpoint.Post.Responses.Status(202) == nil {
		t.Fatal("initialize contract missing or permits a body")
	}
	schema := spec.Components.Schemas["Operation"].Value
	if err := schema.Properties["type"].Value.VisitJSON("REPOSITORY_INITIALIZE"); err != nil {
		t.Fatal(err)
	}
	if err := schema.Properties["type"].Value.VisitJSON("ARBITRARY_COMMAND"); err == nil {
		t.Fatal("arbitrary operation allowed")
	}
	if schema.Properties["repository_id"] == nil {
		t.Fatal("operation has no repository association")
	}
	for _, field := range []string{"restic_password", "gateway_password", "rclone_config", "restic_id"} {
		if schema.Properties[field] != nil {
			t.Fatal("operation exposes private material")
		}
	}
}
