package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gateway"
	"github.com/sagehou/restfleet/internal/rclone"
)

func TestGatewayAuditPersistentChainAndUnauthenticatedFailure(t *testing.T) {
	store, pool, _, _, _ := setupFleet(t)
	ctx := context.Background()
	record, err := gateway.NewAuditRecorder(store)
	if err != nil {
		t.Fatal(err)
	}
	binding := gateway.Binding{HostID: uuid.Must(uuid.NewV7()), RepositoryID: uuid.Must(uuid.NewV7()),
		GatewayID: uuid.Must(uuid.NewV7()), OperationID: uuid.Must(uuid.NewV7())}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			results <- record(ctx, gateway.Event{Binding: binding, Action: "denied", Reason: "path_rejected", Authenticated: true})
		})
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent audit failed")
		}
	}
	if err = record(ctx, gateway.Event{Binding: binding, Action: "denied", Reason: "Bearer secret-canary"}); !errors.Is(err, gateway.ErrGatewayAudit) {
		t.Fatal("unknown event accepted")
	}
	events, err := store.AuditEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if !strings.HasPrefix(event.Action, "GATEWAY_") {
			continue
		}
		count++
		raw, _ := json.Marshal(event)
		if strings.Contains(string(raw), "secret-canary") || event.ActorType != domain.ActorSystem || event.ActorID != uuid.Nil {
			t.Fatal("audit leaked input or claimed Agent identity")
		}
	}
	if count != 9 || store.VerifyAuditChain(ctx) != nil {
		t.Fatal("gateway audit chain invalid")
	}
	root, err := os.MkdirTemp("/dev/shm", "rf-audit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := rclone.NewRuntime(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	supervisor, err := gateway.NewSupervisor(runtime, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Close)
	request := func() int {
		r := httptest.NewRequest(http.MethodGet, "https://gateway.example/secret-canary", nil)
		r.Header.Set("Authorization", "Bearer secret-canary")
		w := httptest.NewRecorder()
		supervisor.ServeHTTP(w, r)
		if strings.Contains(w.Body.String(), "secret-canary") {
			t.Fatal("audit failure exposed input")
		}
		return w.Code
	}
	if request() != http.StatusForbidden {
		t.Fatal("unknown route accepted")
	}
	if _, err = pool.Exec(ctx, `create function reject_gateway_audit() returns trigger language plpgsql as $$ begin
		if new.action like 'GATEWAY_%' then raise exception 'audit-secret-canary'; end if; return new; end $$;
		create trigger reject_gateway_audit before insert on audit_events for each row execute function reject_gateway_audit();`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "drop trigger if exists reject_gateway_audit on audit_events;drop function if exists reject_gateway_audit()")
	})
	if request() != http.StatusServiceUnavailable {
		t.Fatal("database audit failure did not fail closed")
	}
	if err = record(ctx, gateway.Event{Binding: binding, Action: "session_start", Reason: "requested"}); !errors.Is(err, gateway.ErrGatewayAudit) {
		t.Fatal("raw database error escaped")
	}
	var persisted int
	if err = pool.QueryRow(ctx, "select count(*) from audit_events where action like 'GATEWAY_%'").Scan(&persisted); err != nil || persisted != 10 {
		t.Fatal("failed gateway audit partially committed")
	}
	if store.VerifyAuditChain(ctx) != nil {
		t.Fatal("failed audit damaged chain")
	}
}
