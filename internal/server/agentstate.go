package server

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

const (
	agentOfflineAfter = 45 * time.Second
	maxAgentClockSkew = 2 * time.Minute
)

var (
	diagnosticCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	configHashPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	fieldPathPattern      = regexp.MustCompile(`^[A-Za-z0-9_.\[\]-]{0,128}$`)
)

type AgentHealthCounts struct {
	Online   int
	Degraded int
	Offline  int
}

func (c *ControlPlane) withAgentHealth(agents []domain.Agent) []domain.Agent {
	now := c.clock().UTC()
	for i := range agents {
		agents[i].Health = agents[i].HealthAt(now, agentOfflineAfter)
	}
	return agents
}

func (c *ControlPlane) AgentHealthCounts(ctx context.Context) (AgentHealthCounts, error) {
	agents, err := c.Agents(ctx)
	if err != nil {
		return AgentHealthCounts{}, err
	}
	var counts AgentHealthCounts
	for _, agent := range agents {
		switch agent.Health {
		case domain.AgentHealthOnline:
			counts.Online++
		case domain.AgentHealthDegraded:
			counts.Degraded++
		case domain.AgentHealthOffline:
			counts.Offline++
		}
	}
	return counts, nil
}

func (c *ControlPlane) DesiredState(
	ctx context.Context,
	agentID uuid.UUID,
) (domain.DesiredState, error) {
	return c.store.DesiredState(ctx, agentID)
}

func (c *ControlPlane) MarkDesiredStatePublished(
	ctx context.Context,
	agentID uuid.UUID,
	revision int64,
) error {
	return c.store.MarkDesiredStatePublished(ctx, agentID, revision, c.clock().UTC())
}

func (c *ControlPlane) RecordAgentHeartbeat(
	ctx context.Context,
	heartbeat domain.AgentHeartbeat,
) (domain.Agent, error) {
	if heartbeat.AgentID == uuid.Nil || strings.TrimSpace(heartbeat.BootID) == "" ||
		utf8.RuneCountInString(heartbeat.BootID) > 64 ||
		heartbeat.UptimeSeconds < 0 || heartbeat.AcceptedRevision < 0 ||
		heartbeat.StateFreeBytes < 0 || utf8.RuneCountInString(heartbeat.ResticVersion) > 64 ||
		len(heartbeat.HealthChecks) > 32 || heartbeat.LocalTime.IsZero() {
		return domain.Agent{}, &ValidationError{Field: "heartbeat", Code: "INVALID_HEARTBEAT"}
	}
	current, err := c.store.Agent(ctx, heartbeat.AgentID)
	if err != nil {
		return domain.Agent{}, err
	}
	if current.Status != domain.AgentActive {
		return domain.Agent{}, domain.ErrAgentRevoked
	}
	if heartbeat.AcceptedRevision > current.DesiredRevision {
		return domain.Agent{}, &ValidationError{Field: "accepted_revision", Code: "REVISION_AHEAD"}
	}
	for _, check := range heartbeat.HealthChecks {
		if strings.TrimSpace(check.Name) == "" || utf8.RuneCountInString(check.Name) > 64 ||
			(check.ErrorCode != "" && !diagnosticCodePattern.MatchString(check.ErrorCode)) {
			return domain.Agent{}, &ValidationError{Field: "health_checks", Code: "INVALID_HEALTH_CHECK"}
		}
		if !check.Healthy && heartbeat.ErrorCode == "" {
			heartbeat.ErrorCode = check.ErrorCode
			if heartbeat.ErrorCode == "" {
				heartbeat.ErrorCode = "HEALTH_CHECK_FAILED"
			}
		}
	}
	if heartbeat.ClockOffsetMS > maxAgentClockSkew.Milliseconds() ||
		heartbeat.ClockOffsetMS < -maxAgentClockSkew.Milliseconds() {
		heartbeat.ErrorCode = "CLOCK_SKEW"
	}
	heartbeat.ReceivedAt = c.clock().UTC()
	return c.store.RecordAgentHeartbeat(ctx, heartbeat)
}

func (c *ControlPlane) RecordAgentInventory(
	ctx context.Context,
	inventory domain.AgentInventory,
) error {
	if inventory.AgentID == uuid.Nil || inventory.CapturedAt.IsZero() ||
		!validTelemetryText(inventory.Kernel, 256) ||
		!validTelemetryText(inventory.OSRelease, 256) ||
		(inventory.CPUArch != "amd64" && inventory.CPUArch != "arm64") ||
		!validTelemetryText(inventory.AgentVersion, 64) ||
		utf8.RuneCountInString(inventory.ResticVersion) > 64 ||
		len(inventory.AvailableBytes) > 16 || len(inventory.Capabilities) > 64 {
		return &ValidationError{Field: "inventory", Code: "INVALID_INVENTORY"}
	}
	for name, value := range inventory.AvailableBytes {
		if !validTelemetryText(name, 64) {
			return &ValidationError{Field: "available_bytes", Code: "INVALID_PATH_NAME"}
		}
		if value > uint64(^uint64(0)>>1) {
			return &ValidationError{Field: "available_bytes", Code: "VALUE_TOO_LARGE"}
		}
	}
	for _, capability := range inventory.Capabilities {
		if !validTelemetryText(capability, 64) {
			return &ValidationError{Field: "capabilities", Code: "INVALID_CAPABILITY"}
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	inventory.ID = id
	return c.store.RecordAgentInventory(ctx, inventory)
}

func (c *ControlPlane) LatestAgentInventory(
	ctx context.Context,
	hostID uuid.UUID,
) (domain.AgentInventory, error) {
	if _, err := c.store.Host(ctx, hostID); err != nil {
		return domain.AgentInventory{}, err
	}
	return c.store.LatestAgentInventory(ctx, hostID)
}

func (c *ControlPlane) AcceptAgentConfig(
	ctx context.Context,
	agentID uuid.UUID,
	result domain.AgentConfigResult,
) error {
	if result.Revision < 1 || !configHashPattern.MatchString(result.ConfigHash) {
		return &ValidationError{Field: "config_accepted", Code: "INVALID_CONFIG_RESULT"}
	}
	return c.store.AcceptAgentConfig(ctx, agentID, result, c.clock().UTC())
}

func (c *ControlPlane) RejectAgentConfig(
	ctx context.Context,
	agentID uuid.UUID,
	result domain.AgentConfigResult,
) error {
	if result.Revision < 1 || !diagnosticCodePattern.MatchString(result.ErrorCode) ||
		!fieldPathPattern.MatchString(result.FieldPath) {
		return &ValidationError{Field: "config_rejected", Code: "INVALID_CONFIG_RESULT"}
	}
	return c.store.RejectAgentConfig(ctx, agentID, result, c.clock().UTC())
}

func validTelemetryText(value string, maxRunes int) bool {
	return strings.TrimSpace(value) != "" && utf8.RuneCountInString(value) <= maxRunes
}
