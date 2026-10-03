package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

// Real Unix exchange, pinned central/source keys and queue initialization.
// Only the authority/material transaction is a fixture in this package; actual
// PostgreSQL current-state and rollback behavior has separate integration tests.
func deliverGatewayOwner(t *testing.T, s *Supervisor, grant security.GatewayStatement, central ed25519.PrivateKey, pending [32]byte) (*AuthorizedBackup, *gatewaypending.Queue, ed25519.PublicKey) {
	t.Helper()
	sourcePublic, source, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(source)
	public := central.Public().(ed25519.PublicKey)
	receiver, err := gatewaypending.NewMaterialReceiver(grant.Binding, source, public)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	dir, err := os.MkdirTemp("", "rfg-delivery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "material.sock")
	listener, err := gatewaypending.ListenReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	queuePath := t.TempDir()
	if os.Chmod(queuePath, 0700) != nil {
		t.Fatal("queue directory")
	}
	var owner *AuthorizedBackup
	var queue *gatewaypending.Queue
	var borrowed []byte
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- receiver.Receive(ctx, listener, uint32(os.Geteuid()), func(_ context.Context, m security.GatewayMaterial) (func(), error) {
			borrowed = m.Config
			var installErr error
			owner, queue, installErr = InstallGatewayMaterial(s, queuePath, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 64}, m, source, public)
			if installErr != nil {
				return nil, installErr
			}
			return owner.Close, nil
		}, func(context.Context) error { return nil })
	}()
	err = gatewaypending.SendMaterial(ctx, path, uint32(os.Geteuid()), grant.Binding, sourcePublic, func(_ context.Context, proof []byte) ([]byte, error) {
		challenge, err := security.VerifyGatewayMaterialChallenge(proof, sourcePublic, grant.Binding)
		if err != nil {
			return nil, err
		}
		statement, err := security.SignGatewayStatement(grant, central)
		if err != nil {
			return nil, err
		}
		config, err := rclone.ParseConfig(string(backupFixture().Config), "encrypted")
		if err != nil {
			return nil, err
		}
		raw := config.Bytes()
		defer clear(raw)
		return security.SealGatewayMaterial(security.GatewayMaterial{Challenge: challenge, Source: sourcePublic, PendingRecipient: pending, Statement: statement,
			AdmissionCreatedAt: grant.IssuedAt - 1, AdmissionExpiresAt: grant.ExpiresAt, SecretRevision: 1, Remote: "encrypted", ConfigHash: security.GatewayPendingHash(raw), Config: raw}, central)
	}, func(context.Context) error { return nil })
	receiveErr := <-done
	if err != nil || receiveErr != nil || owner == nil || queue == nil || len(bytes.Trim(borrowed, "\x00")) != 0 {
		t.Fatal("authenticated material installation failed")
	}
	t.Cleanup(func() { owner.Close(); _ = queue.Close() })
	return owner, queue, sourcePublic
}

func TestDeliveredGatewayOwnerRunsAndClearsMaterial(t *testing.T) {
	_, _, grant, central, _, pendingPrivate := pendingGatewayFixture(t, gatewaypending.Limits{MaxBytes: 2 << 20, MaxRecords: 64})
	pending, err := security.GatewayPendingPublicKey(pendingPrivate)
	if err != nil {
		t.Fatal(err)
	}
	s, state, root := supervisorFixture(t, "success", 1)
	owner, queue, source := deliverGatewayOwner(t, s, grant, central, pending)
	s.audit = func(context.Context, Event) error { return ErrGatewayAudit }
	for range 2 {
		op := startAuthorized(t, owner, nil)
		_ = waitAccess(t, op)
		finishSupervised(t, op)
		assertSupervisorClean(t, state, root, backupFixture())
	}
	owner.Close()
	if len(drainAuthorized(t, queue, central, source, pendingPrivate)) != 4 {
		t.Fatal("delivered owner did not preserve independent session records")
	}
}
