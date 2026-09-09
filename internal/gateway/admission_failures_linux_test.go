package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmittedBackupEarlyFailuresNeverRelease(t *testing.T) {
	for _, mode := range []string{"start-audit", "backend", "material", "callback"} {
		t.Run(mode, func(t *testing.T) {
			engine := "success"
			if mode == "backend" {
				engine = "exit"
			}
			s, state, root := supervisorFixture(t, engine, 1)
			f := newAdmissionStoreFixture()
			request := backupFixture()
			if mode == "material" {
				request.Config = []byte("invalid")
			}
			if mode == "start-audit" {
				s.audit = func(context.Context, Event) error { return errors.New("audit-secret-canary") }
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			err := s.WithAdmittedBackup(ctx, f, f.grant.ID, f.grant.Owner, f.grant.ConfigurationHash, request,
				noGatewayRefresh, func(context.Context, Access) error { return errors.New("callback-secret-canary") })
			if err == nil || f.releases != 0 {
				t.Fatal("failed run released its fence")
			}
			assertSupervisorClean(t, state, root, request)
		})
	}
}
