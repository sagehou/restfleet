package httpapi

import (
	"net/http"
	"testing"

	"github.com/sagehou/restfleet/internal/persistence/postgres"
)

func TestVersionReportsExpectedSchema(t *testing.T) {
	_, _, _, b, _ := setupFleet(t)
	response := b.request(t, http.MethodGet, "/api/v1/version", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("version status = %d, body = %s", response.Code, response.Body.String())
	}
	var version Version
	decodeResponse(t, response, &version)
	if version.SchemaVersion != postgres.ExpectedSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version.SchemaVersion, postgres.ExpectedSchemaVersion)
	}
}
