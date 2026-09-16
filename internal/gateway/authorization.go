package gateway

import (
	"crypto/ed25519"
	"errors"
	"sync"
	"time"

	"github.com/sagehou/restfleet/internal/security"
)

type AuthorizationStatus string

const (
	AuthorizationUnknown     AuthorizationStatus = "UNKNOWN"
	AuthorizationValid       AuthorizationStatus = "VALID"
	AuthorizationExpired     AuthorizationStatus = "EXPIRED"
	AuthorizationRevoked     AuthorizationStatus = "REVOKED"
	AuthorizationClockUnsafe AuthorizationStatus = "CLOCK_UNSAFE"
)

var ErrAuthorization = errors.New("gateway authorization unavailable or inconsistent")

// Authorization tracks verified decisions for ONE immutable runtime binding.
// Connectivity belongs to the transport, not this state: a timeout cannot
// revoke or renew authority. This is NOT yet wired to production admission.
// Callers MUST use one instance for the binding's lifetime, never recreate it
// to clear revocation/rollback protection, and obtain fresh central admission
// after restart. No method releases a durable fence or proves backend cleanup.
type Authorization struct {
	mu          sync.Mutex
	key         ed25519.PublicKey
	binding     security.GatewayAuthorizationBinding
	current     security.GatewayStatement
	lastWall    time.Time
	deadline    time.Time
	clockUnsafe bool
	clock       func() time.Time // Test clock; sampled under mu, never supplied by a request.
}

func NewAuthorization(key ed25519.PublicKey, binding security.GatewayAuthorizationBinding) (*Authorization, error) {
	if len(key) != ed25519.PublicKeySize || binding.Validate() != nil {
		return nil, ErrAuthorization
	}
	return &Authorization{key: append(ed25519.PublicKey(nil), key...), binding: binding}, nil
}

// Accept receives signed central decisions only. Time is sampled under the lock
// so concurrent callers cannot manufacture clock rollback. A reconnect alone cannot
// invoke a transition. Exact replay is idempotent but never resets a deadline.
func (a *Authorization) Accept(wire []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.sampleTime()
	if !a.observeClock(now) {
		return ErrAuthorization
	}
	s, err := security.VerifyGatewayStatement(wire, a.key)
	if err != nil || s.Binding != a.binding || s.IssuedAt > now.Unix() {
		return ErrAuthorization
	}
	if s == a.current {
		return nil
	}
	if a.current.Revoked || s.Revision <= a.current.Revision || s.IssuedAt < a.current.IssuedAt ||
		(!s.Revoked && s.ExpiresAt <= now.Unix()) {
		return ErrAuthorization
	}
	a.current = s
	if !s.Revoked {
		// Preserve time.Now's monotonic component; receiving an old grant does
		// not grant another 12h. Wall-clock expiry is also checked in Status.
		a.deadline = now.Add(time.Unix(s.ExpiresAt, 0).Sub(now))
	}
	return nil
}

// Status is local knowledge, not a claim about an unreachable central DB.
// Invalid input does not erase an accepted decision. A verified revocation is
// terminal even if the clock subsequently fails; expiry is not revocation.
func (a *Authorization) Status() AuthorizationStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.sampleTime()
	clockOK := a.observeClock(now)
	if a.current.Revoked {
		return AuthorizationRevoked
	}
	if !clockOK {
		return AuthorizationClockUnsafe
	}
	if a.current.Revision == 0 {
		return AuthorizationUnknown
	}
	if now.Unix() >= a.current.ExpiresAt || !now.Before(a.deadline) {
		return AuthorizationExpired
	}
	return AuthorizationValid
}

func (a *Authorization) observeClock(now time.Time) bool {
	wall := now.Round(0)
	if now.IsZero() || wall.Before(a.lastWall) {
		a.clockUnsafe = true
	}
	a.lastWall = wall
	return !a.clockUnsafe
}

func (a *Authorization) sampleTime() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}
