package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOfflineAuthorizationRequestValidate(t *testing.T) {
	valid := func() OfflineAuthorizationRequest {
		id, _ := uuid.NewV7()
		owner, _ := uuid.NewV7()
		gw, _ := uuid.NewV7()
		agent, _ := uuid.NewV7()
		delivery, _ := uuid.NewV7()
		return OfflineAuthorizationRequest{
			ID: id, Owner: owner, GatewayInstanceID: gw, AgentID: agent, DeliveryID: delivery,
			ConfigurationHash: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			Lifetime:          time.Hour,
		}
	}

	t.Run("valid", func(t *testing.T) {
		r := valid()
		if err := r.Validate(); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("zero_id", func(t *testing.T) {
		r := valid()
		r.ID = uuid.Nil
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for nil ID")
		}
	})

	t.Run("bad_hash_length", func(t *testing.T) {
		r := valid()
		r.ConfigurationHash = "abc"
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for short hash")
		}
	})

	t.Run("bad_hash_chars", func(t *testing.T) {
		r := valid()
		r.ConfigurationHash = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for non-hex hash")
		}
	})

	t.Run("lifetime_too_short", func(t *testing.T) {
		r := valid()
		r.Lifetime = 30 * time.Second
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for <1m lifetime")
		}
	})

	t.Run("lifetime_too_long", func(t *testing.T) {
		r := valid()
		r.Lifetime = 13 * time.Hour
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for >12h lifetime")
		}
	})

	t.Run("lifetime_sub_second", func(t *testing.T) {
		r := valid()
		r.Lifetime = time.Minute + 500*time.Millisecond
		if err := r.Validate(); err == nil {
			t.Fatal("expected error for sub-second precision")
		}
	})
}

func TestOfflineAuthorizationActive(t *testing.T) {
	now := time.Now().UTC()
	a := OfflineAuthorization{
		AuthorizedAt: now.Add(-time.Hour),
		ExpiresAt:    now.Add(time.Hour),
	}

	if !a.Active(now) {
		t.Fatal("expected active")
	}
	if a.Active(now.Add(2 * time.Hour)) {
		t.Fatal("expected not active after expiry")
	}

	revoked := a
	revoked.RevokedAt = &now
	if revoked.Active(now) {
		t.Fatal("expected not active when revoked")
	}

	disabled := a
	disabled.DisabledAt = &now
	if disabled.Active(now) {
		t.Fatal("expected not active when disabled")
	}
}

func TestOfflineAuthorizationRemaining(t *testing.T) {
	now := time.Now().UTC()
	a := OfflineAuthorization{
		AuthorizedAt: now,
		ExpiresAt:    now.Add(2 * time.Hour),
	}

	remaining := a.RemainingAuthorization(now)
	if remaining < time.Hour || remaining > 2*time.Hour {
		t.Fatalf("expected ~2h remaining, got %v", remaining)
	}

	if a.RemainingAuthorization(now.Add(3*time.Hour)) != 0 {
		t.Fatal("expected 0 remaining after expiry")
	}
}

func TestOfflineRenewalRequestValidate(t *testing.T) {
	valid := func() OfflineRenewalRequest {
		id, _ := uuid.NewV7()
		owner, _ := uuid.NewV7()
		return OfflineRenewalRequest{
			AuthorizationID:   id,
			Owner:             owner,
			CurrentSequence:   1,
			NewExpiresAt:      time.Now().UTC().Add(time.Hour),
			ConfigurationHash: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		}
	}

	t.Run("valid", func(t *testing.T) {
		r := valid()
		if err := r.Validate(12 * time.Hour); err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
	})

	t.Run("zero_sequence", func(t *testing.T) {
		r := valid()
		r.CurrentSequence = 0
		if err := r.Validate(0); err == nil {
			t.Fatal("expected error for zero sequence")
		}
	})

	t.Run("exceed_max_lifetime", func(t *testing.T) {
		r := valid()
		r.NewExpiresAt = time.Now().UTC().Add(13 * time.Hour)
		if err := r.Validate(12 * time.Hour); err == nil {
			t.Fatal("expected error for exceeding max lifetime")
		}
	})
}
