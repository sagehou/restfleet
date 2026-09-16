package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

type decisionTestStore struct {
	Store
	decision domain.GatewayDecision
	err      error
	calls    int
}

func (s *decisionTestStore) DecideGatewayAuthorization(context.Context, domain.GatewayDecisionRequest, string) (domain.GatewayDecision, error) {
	s.calls++
	return s.decision, s.err
}

func TestCentralGatewaySigningRequiresCommittedMatchingDecision(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ca, caKey, err := security.NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	clear(caKey)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	store := &decisionTestStore{}
	settings := Settings{MasterKey: bytes.Repeat([]byte{8}, 32), GatewayPublicURL: "https://gateway.example", GatewaySigningKey: key,
		Clock: func() time.Time { return now }, Enrollment: EnrollmentSettings{ServerCABundlePEM: ca.CertificatePEM()},
		PasswordParams: security.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}}
	c, err := NewControlPlane(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	r := domain.GatewayDecisionRequest{ID: id, AdmissionID: id, Owner: id, RuntimeID: id, Lifetime: time.Hour}
	a := domain.BackupAdmission{ID: id, Owner: id, AgentID: id, HostID: id, RepositoryID: id, GatewayID: id, StorageCredentialID: id,
		DeliveryID: id, GatewaySecretRef: id, ResticSecretRef: id, ConfigurationHash: c.credentialConfigurationHash(), ExpiresAt: now.Add(2 * time.Hour)}
	good := domain.GatewayDecision{RequestID: id, RuntimeID: id, Admission: a, Revision: 1, RequestedLifetime: time.Hour, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	store.decision = good
	wire, err := c.DecideGatewayAuthorization(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = security.VerifyGatewayStatement(wire, public); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.GatewayDecision){
		func(d *domain.GatewayDecision) { d.RequestID = uuid.Nil }, func(d *domain.GatewayDecision) { d.RuntimeID = uuid.Nil },
		func(d *domain.GatewayDecision) { d.Admission.Owner = uuid.Nil }, func(d *domain.GatewayDecision) { d.Revision = 2 },
		func(d *domain.GatewayDecision) { d.Revoked = true }, func(d *domain.GatewayDecision) { d.IssuedAt = now.Add(time.Second) },
		func(d *domain.GatewayDecision) { d.ExpiresAt = now }, func(d *domain.GatewayDecision) { d.ExpiresAt = now.Add(3 * time.Hour) },
		func(d *domain.GatewayDecision) { d.Admission.ConfigurationHash = strings.Repeat("b", 64) },
		func(d *domain.GatewayDecision) { d.Admission.ReleasedAt = &now },
	} {
		store.decision = good
		mutate(&store.decision)
		if wire, err := c.DecideGatewayAuthorization(context.Background(), r); err != domain.ErrGatewayDecision || wire != nil {
			t.Fatal("unverified store result signed")
		}
	}
	store.decision = good
	store.err = errors.New("database-secret-canary")
	if wire, err := c.DecideGatewayAuthorization(context.Background(), r); err != domain.ErrGatewayDecision || wire != nil {
		t.Fatal("commit error exposed signature or raw error")
	}
	store.err = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if wire, err := c.DecideGatewayAuthorization(ctx, r); err != domain.ErrGatewayDecision || wire != nil {
		t.Fatal("canceled call signed")
	}
	settings.GatewaySigningKey = nil
	disabled, err := NewControlPlane(store, settings)
	if err != nil {
		t.Fatal(err)
	}
	before := store.calls
	if wire, err := disabled.DecideGatewayAuthorization(context.Background(), r); err != domain.ErrGatewayDecision || wire != nil || store.calls != before {
		t.Fatal("disabled signer committed new decision")
	}
	for _, bad := range []ed25519.PrivateKey{make([]byte, 63), make([]byte, 64), make([]byte, 65)} {
		settings.GatewaySigningKey = bad
		if _, err := NewControlPlane(store, settings); err != domain.ErrGatewayDecision {
			t.Fatal("invalid signing key accepted")
		}
	}
}
