package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDefaultDesiredStateCanonicalHash(t *testing.T) {
	state, err := NewDefaultDesiredState(uuid.Must(uuid.NewV7()), 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	const expected = "sha256:16db572cf66c08d0db851c5cba74041a651367e1a0086c50d05223576539e0fb"
	if state.ConfigHash != expected {
		t.Fatalf("config hash = %q, want %q", state.ConfigHash, expected)
	}
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentHealthSeparatesIdentityFromLiveness(t *testing.T) {
	now := time.Now().UTC()
	seen := now.Add(-10 * time.Second)
	agent := Agent{Status: AgentActive, LastSeenAt: &seen}
	if got := agent.HealthAt(now, 45*time.Second); got != AgentHealthOnline {
		t.Fatalf("fresh health = %q", got)
	}
	agent.ConfigErrorCode = "CONFIG_HASH_MISMATCH"
	if got := agent.HealthAt(now, 45*time.Second); got != AgentHealthDegraded {
		t.Fatalf("degraded health = %q", got)
	}
	agent.ConfigErrorCode = ""
	seen = now.Add(-46 * time.Second)
	if got := agent.HealthAt(now, 45*time.Second); got != AgentHealthOffline {
		t.Fatalf("stale health = %q", got)
	}
	agent.Status = AgentRevoked
	if got := agent.HealthAt(now, 45*time.Second); got != AgentRevoked {
		t.Fatalf("revoked health = %q", got)
	}
}
