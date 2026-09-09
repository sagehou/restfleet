package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

type auditStoreFunc func(context.Context, domain.AuditEvent) error

func (f auditStoreFunc) RecordAudit(ctx context.Context, event domain.AuditEvent) error {
	return f(ctx, event)
}

func TestGatewayAuditFixedClassifications(t *testing.T) {
	events := []Event{
		{Action: "denied", Reason: "route_unavailable"},
		{Action: "denied", Reason: "rate_limited"},
		{Binding: bindingFixture(), Action: "session_start", Reason: "requested"},
		{Binding: bindingFixture(), Action: "session_end", Reason: "finished"},
		{Binding: bindingFixture(), Authenticated: true, Action: "lock_cleanup", Reason: "owned_lock"},
	}
	for _, reason := range []string{"session_inactive", "tls_required", "authentication_failed"} {
		events = append(events, Event{Binding: bindingFixture(), Action: "denied", Reason: reason})
	}
	for _, reason := range []string{"path_rejected", "request_limit", "method_rejected", "immutable_object", "lock_not_fresh", "object_size", "invalid_body", "content_hash_mismatch", "object_exists", "lock_not_owned"} {
		events = append(events, Event{Binding: bindingFixture(), Authenticated: true, Action: "denied", Reason: reason})
	}
	for _, event := range events {
		t.Run(event.Reason, func(t *testing.T) {
			called := false
			record, err := NewAuditRecorder(auditStoreFunc(func(ctx context.Context, a domain.AuditEvent) error {
				called = true
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Fatal("audit not bounded")
				}
				if a.ID.Version() != 7 || a.ID.Variant() != uuid.RFC4122 || a.RequestID != a.ID || a.OccurredAt.Location() != time.UTC {
					t.Fatal("invalid audit identity/time")
				}
				if a.ActorType != domain.ActorSystem || a.ActorID != uuid.Nil || len(a.SourceIPHash) != 0 {
					t.Fatal("request falsely attributed")
				}
				if a.ReasonCode != strings.ToUpper(event.Reason) {
					t.Fatal("reason changed")
				}
				if event.Binding == (Binding{}) {
					if a.ResourceID != uuid.Nil || len(a.Changes) != 0 {
						t.Fatal("unrouted event gained identity")
					}
				} else {
					var fields map[string]any
					if json.Unmarshal(a.Changes, &fields) != nil || len(fields) != 4 ||
						fields["route_host_id"] != event.Binding.HostID.String() || fields["gateway_id"] != event.Binding.GatewayID.String() ||
						fields["session_id"] != event.Binding.OperationID.String() || fields["authenticated"] != event.Authenticated {
						t.Fatal("unexpected audit fields")
					}
					if a.ResourceID != event.Binding.RepositoryID {
						t.Fatal("wrong resource")
					}
				}
				if event.Action == "lock_cleanup" && a.Action != "GATEWAY_LOCK_CLEANUP_INTENT" {
					t.Fatal("intent claimed completion")
				}
				if event.Action == "denied" && a.Result != domain.AuditDenied {
					t.Fatal("denial claimed success")
				}
				return nil
			}))
			if err != nil || record(context.Background(), event) != nil || !called {
				t.Fatal("valid event rejected")
			}
		})
	}
}

func TestGatewayAuditRejectsUnknownAndContradictoryInputsWithoutEcho(t *testing.T) {
	scoped := Event{Binding: bindingFixture(), Action: "denied", Reason: "path_rejected", Authenticated: true}
	for _, mutate := range []func(*Event){
		func(e *Event) { e.Action = "Bearer secret-canary" },
		func(e *Event) { e.Reason = "https://user:secret-canary@example.test" },
		func(e *Event) { e.Binding.HostID = uuid.Nil },
		func(e *Event) { e.Binding.RepositoryID = uuid.New() },
		func(e *Event) { e.Authenticated = false },
		func(e *Event) { e.Reason = "rate_limited" },
		func(e *Event) { e.Binding = Binding{} },
		func(e *Event) { e.Action = "lock_cleanup"; e.Reason = "owned_lock"; e.Authenticated = false },
		func(e *Event) { e.Action = "session_start"; e.Reason = "requested" },
	} {
		event := scoped
		mutate(&event)
		var saved domain.AuditEvent
		record, err := NewAuditRecorder(auditStoreFunc(func(_ context.Context, a domain.AuditEvent) error { saved = a; return nil }))
		if err != nil {
			t.Fatal(err)
		}
		err = record(context.Background(), event)
		if !errors.Is(err, ErrGatewayAudit) {
			t.Fatal("invalid event accepted")
		}
		raw, _ := json.Marshal(saved)
		if strings.Contains(string(raw), "secret-canary") || saved.Action != "GATEWAY_EVENT_REJECTED" || saved.ReasonCode != "INVALID_EVENT" ||
			saved.ResourceID != uuid.Nil || len(saved.Changes) != 0 {
			t.Fatal("invalid event leaked context")
		}
	}
}

func TestGatewayAuditFailureAndCancellationAreFailClosed(t *testing.T) {
	if _, err := NewAuditRecorder(nil); err == nil {
		t.Fatal("nil store accepted")
	}
	record, err := NewAuditRecorder(auditStoreFunc(func(context.Context, domain.AuditEvent) error { return errors.New("database-secret-canary") }))
	if err != nil {
		t.Fatal(err)
	}
	if err = record(context.Background(), Event{Action: "denied", Reason: "rate_limited"}); !errors.Is(err, ErrGatewayAudit) {
		t.Fatal("database error escaped")
	}
	record, err = NewAuditRecorder(auditStoreFunc(func(ctx context.Context, _ domain.AuditEvent) error { <-ctx.Done(); return ctx.Err() }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err = record(ctx, Event{Action: "denied", Reason: "route_unavailable"}); !errors.Is(err, ErrGatewayAudit) {
		t.Fatal("canceled audit accepted")
	}
}

func TestGatewayAuditAdapterPreventsUnauditedLockDeletion(t *testing.T) {
	s, secret, backend := fixture(t)
	path := object("locks", "audit-intent-fixture")
	serve(t, s, request(s, secret, http.MethodPost, path, "audit-intent-fixture"), http.StatusOK)
	var err error
	s.audit, err = NewAuditRecorder(auditStoreFunc(func(context.Context, domain.AuditEvent) error { return errors.New("secret-canary") }))
	if err != nil {
		t.Fatal(err)
	}
	serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusServiceUnavailable)
	if backend.inspect().objects[path] != "audit-intent-fixture" {
		t.Fatal("lock deleted before durable audit")
	}
	s.audit, err = NewAuditRecorder(auditStoreFunc(func(_ context.Context, e domain.AuditEvent) error {
		if e.Action != "GATEWAY_LOCK_CLEANUP_INTENT" || backend.inspect().objects[path] != "audit-intent-fixture" {
			t.Error("cleanup was not audited before deletion")
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	serve(t, s, request(s, secret, http.MethodDelete, path, ""), http.StatusOK)
}
