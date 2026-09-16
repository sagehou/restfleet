package gateway

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

func authorizationFixture(t *testing.T) (*Authorization, security.GatewayStatement, ed25519.PrivateKey, time.Time) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse("019abcde-1234-7000-8000-000000000001")
	now := time.Unix(1800000000, 0)
	s := security.GatewayStatement{Binding: security.GatewayAuthorizationBinding{
		AdmissionID: id, Owner: id, RuntimeID: id, AgentID: id, HostID: id, RepositoryID: id,
		GatewayID: id, StorageCredentialID: id, DeliveryID: id, GatewaySecretRef: id, ResticSecretRef: id,
		ConfigurationHash: strings.Repeat("a", 64),
	}, Revision: 1, IssuedAt: now.Unix(), ExpiresAt: now.Add(12 * time.Hour).Unix()}
	a, err := NewAuthorization(public, s.Binding)
	if err != nil {
		t.Fatal(err)
	}
	clear(public) // Constructor must own its verification key.
	return a, s, private, now
}

func statementWire(t *testing.T, s security.GatewayStatement, key ed25519.PrivateKey) []byte {
	t.Helper()
	wire, err := security.SignGatewayStatement(s, key)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestAuthorizationDisconnectDoesNotRevokeOrExtend(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	if a.statusAt(now) != AuthorizationUnknown {
		t.Fatal("missing grant authorized")
	}
	wire := statementWire(t, s, key)
	// Delivery is delayed by an hour; no new central decisions arrive offline.
	if err := a.acceptAt(wire, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for hour := 1; hour < 12; hour++ {
		at := now.Add(time.Duration(hour) * time.Hour)
		if a.statusAt(at) != AuthorizationValid {
			t.Fatal("disconnect revoked a valid grant")
		}
		if err := a.acceptAt(wire, at); err != nil {
			t.Fatal(err)
		}
	}
	at := now.Add(12 * time.Hour)
	if a.statusAt(at) != AuthorizationExpired {
		t.Fatal("expiry extended or called revocation")
	}
	if err := a.acceptAt(wire, at); err != nil {
		t.Fatal("exact replay not idempotent")
	}
	if a.statusAt(at) != AuthorizationExpired {
		t.Fatal("replay resurrected expiry")
	}
}

func TestAuthorizationRevocationIsTerminal(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	old := statementWire(t, s, key)
	if err := a.acceptAt(old, now); err != nil {
		t.Fatal(err)
	}
	s.Revision++
	s.Revoked, s.ExpiresAt = true, 0
	revoked := statementWire(t, s, key)
	if err := a.acceptAt(revoked, now); err != nil {
		t.Fatal(err)
	}
	if err := a.acceptAt(revoked, now); err != nil {
		t.Fatal("revocation replay not idempotent")
	}
	if a.statusAt(now) != AuthorizationRevoked {
		t.Fatal("revocation not applied")
	}
	if err := a.acceptAt(old, now); err != ErrAuthorization {
		t.Fatal("stale grant revived revocation")
	}
	s.Revision++
	s.Revoked, s.ExpiresAt = false, now.Add(time.Hour).Unix()
	if err := a.acceptAt(statementWire(t, s, key), now); err != ErrAuthorization {
		t.Fatal("new grant revived terminal binding")
	}
	if a.statusAt(now.Add(-time.Second)) != AuthorizationRevoked {
		t.Fatal("clock rollback hid known revocation")
	}
}

func TestAuthorizationRenewalAndRejectedInput(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	if err := a.acceptAt(statementWire(t, s, key), now); err != nil {
		t.Fatal(err)
	}
	next := s
	next.Revision++
	next.IssuedAt += 60
	next.ExpiresAt += 60
	now = now.Add(time.Minute)
	if err := a.acceptAt(statementWire(t, next, key), now); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*security.GatewayStatement){
		func(s *security.GatewayStatement) { s.Revision-- },
		func(s *security.GatewayStatement) { s.ExpiresAt-- }, // Same revision, conflicting payload.
		func(s *security.GatewayStatement) { s.Revision++; s.IssuedAt--; s.ExpiresAt-- },
		func(s *security.GatewayStatement) { s.Revision++; s.IssuedAt++; s.ExpiresAt++ },
		func(s *security.GatewayStatement) { s.Binding.HostID = uuid.Must(uuid.NewV7()) },
		func(s *security.GatewayStatement) { s.Binding.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(s *security.GatewayStatement) { s.Binding.AdmissionID = uuid.Must(uuid.NewV7()) },
		func(s *security.GatewayStatement) { s.Binding.Owner = uuid.Must(uuid.NewV7()) },
		func(s *security.GatewayStatement) { s.Binding.ConfigurationHash = strings.Repeat("b", 64) },
	} {
		bad := next
		change(&bad)
		if err := a.acceptAt(statementWire(t, bad, key), now); err != ErrAuthorization {
			t.Fatal("unsafe transition accepted")
		}
		if a.statusAt(now) != AuthorizationValid {
			t.Fatal("invalid input erased accepted grant")
		}
	}
	if err := a.acceptAt([]byte("secret-canary"), now); err != ErrAuthorization {
		t.Fatal("unsigned input accepted")
	}
	if a.statusAt(time.Unix(s.ExpiresAt, 0)) != AuthorizationValid {
		t.Fatal("central renewal not applied")
	}
	if a.statusAt(time.Unix(next.ExpiresAt, 0)) != AuthorizationExpired {
		t.Fatal("renewed expiry ignored")
	}
}

func TestAuthorizationClockFailureAndInitialBounds(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	if err := a.acceptAt(statementWire(t, s, key), now); err != nil {
		t.Fatal(err)
	}
	if a.statusAt(now.Add(-time.Nanosecond)) != AuthorizationClockUnsafe {
		t.Fatal("clock rollback ignored")
	}
	if a.statusAt(now.Add(time.Hour)) != AuthorizationClockUnsafe {
		t.Fatal("clock failure cleared itself")
	}
	if err := a.acceptAt(statementWire(t, s, key), now.Add(time.Hour)); err != ErrAuthorization {
		t.Fatal("clock failure bypassed")
	}
	b, s, key, now := authorizationFixture(t)
	if err := b.acceptAt(statementWire(t, s, key), now.Add(12*time.Hour)); err != ErrAuthorization {
		t.Fatal("expired initial grant accepted")
	}
	c, s, key, now := authorizationFixture(t)
	if err := c.acceptAt(statementWire(t, s, key), now.Add(-time.Second)); err != ErrAuthorization {
		t.Fatal("future grant accepted")
	}
	if c.statusAt(time.Time{}) != AuthorizationClockUnsafe {
		t.Fatal("zero clock accepted")
	}
	if _, err := NewAuthorization(nil, s.Binding); err != ErrAuthorization {
		t.Fatal("missing key accepted")
	}
}

func TestAuthorizationConcurrentReplayAndRevocation(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	wire := statementWire(t, s, key)
	if err := a.acceptAt(wire, now); err != nil {
		t.Fatal(err)
	}
	s.Revision++
	s.Revoked, s.ExpiresAt = true, 0
	revoked := statementWire(t, s, key)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			err := a.acceptAt(wire, now)
			if err != nil && err != ErrAuthorization {
				t.Error(err)
			}
			status := a.statusAt(now)
			if status != AuthorizationValid && status != AuthorizationRevoked {
				t.Error("unexpected state")
			}
		})
	}
	if err := a.acceptAt(revoked, now); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if a.statusAt(now) != AuthorizationRevoked {
		t.Fatal("concurrent replay revived revocation")
	}
}

func (a *Authorization) acceptAt(wire []byte, now time.Time) error {
	a.mu.Lock()
	a.clock = func() time.Time { return now }
	a.mu.Unlock()
	return a.Accept(wire)
}

func (a *Authorization) statusAt(now time.Time) AuthorizationStatus {
	a.mu.Lock()
	a.clock = func() time.Time { return now }
	a.mu.Unlock()
	return a.Status()
}

func TestAuthorizationConcurrentRealClock(t *testing.T) {
	a, s, key, _ := authorizationFixture(t)
	now := time.Now()
	s.IssuedAt = now.Unix()
	s.ExpiresAt = now.Add(time.Hour).Unix()
	wire := statementWire(t, s, key)
	if err := a.Accept(wire); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if err := a.Accept(wire); err != nil {
				t.Error(err)
			}
			if a.Status() != AuthorizationValid {
				t.Error("concurrent sampling caused false rollback")
			}
		})
	}
	wg.Wait()
}

func TestAuthorizationRejectsEveryForeignBinding(t *testing.T) {
	for i := range 11 {
		a, s, key, now := authorizationFixture(t)
		b := &s.Binding
		ids := []*uuid.UUID{&b.AdmissionID, &b.Owner, &b.RuntimeID, &b.AgentID, &b.HostID, &b.RepositoryID,
			&b.GatewayID, &b.StorageCredentialID, &b.DeliveryID, &b.GatewaySecretRef, &b.ResticSecretRef}
		*ids[i] = uuid.Must(uuid.NewV7())
		if err := a.acceptAt(statementWire(t, s, key), now); err != ErrAuthorization {
			t.Fatalf("foreign binding accepted: %d", i)
		}
		if a.statusAt(now) != AuthorizationUnknown {
			t.Fatal("rejected grant changed state")
		}
	}
}

func TestAuthorizationRevocationWithoutPriorGrant(t *testing.T) {
	a, s, key, now := authorizationFixture(t)
	s.Revoked, s.ExpiresAt = true, 0
	if err := a.acceptAt(statementWire(t, s, key), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a.statusAt(now.Add(24*time.Hour)) != AuthorizationRevoked {
		t.Fatal("delayed revocation ignored")
	}
}
