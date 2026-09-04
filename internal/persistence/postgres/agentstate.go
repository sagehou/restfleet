package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sagehou/restfleet/internal/domain"
)

func (s *Store) Agents(ctx context.Context) ([]domain.Agent, error) {
	rows, err := s.pool.Query(ctx, agentSelect+" order by agents.created_at, agents.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	agents := make([]domain.Agent, 0)
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func (s *Store) DesiredState(ctx context.Context, agentID uuid.UUID) (domain.DesiredState, error) {
	var state domain.DesiredState
	var configJSON []byte
	err := s.pool.QueryRow(ctx, `
		select d.agent_id, d.revision, d.generated_at, d.config_hash, d.config_json
		from agent_desired_states d
		join agents a on a.id = d.agent_id and a.desired_revision = d.revision
		where d.agent_id = $1 and a.status = 'ACTIVE'
	`, agentID).Scan(
		&state.AgentID, &state.Revision, &state.GeneratedAt, &state.ConfigHash,
		&configJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DesiredState{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.DesiredState{}, err
	}
	var config struct {
		RuntimePolicy domain.RuntimePolicy `json:"runtime_policy"`
	}
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return domain.DesiredState{}, err
	}
	state.RuntimePolicy = config.RuntimePolicy
	if err := state.Validate(); err != nil {
		return domain.DesiredState{}, err
	}
	return state, nil
}

func (s *Store) MarkDesiredStatePublished(
	ctx context.Context,
	agentID uuid.UUID,
	revision int64,
	at time.Time,
) error {
	_, err := s.pool.Exec(ctx, `
		update outbox_events
		set published_at = coalesce(published_at, $3),
		    lease_owner = null, lease_expires_at = null
		where aggregate_type = 'AGENT' and aggregate_id = $1
		  and event_type = 'AGENT_DESIRED_STATE_CHANGED'
		  and (payload ->> 'revision')::bigint = $2
	`, agentID, revision, at)
	return err
}

func (s *Store) RecordAgentHeartbeat(
	ctx context.Context,
	heartbeat domain.AgentHeartbeat,
) (domain.Agent, error) {
	tag, err := s.pool.Exec(ctx, `
		update agents
		set boot_id = $2, uptime_seconds = $3, restic_version = $4,
		    accepted_revision = greatest(accepted_revision, $5),
		    state_free_bytes = $6, clock_offset_ms = $7,
		    heartbeat_error_code = $8, last_seen_at = $9, updated_at = $9
		where id = $1 and status = 'ACTIVE'
		  and $5 >= 0 and $5 <= desired_revision
	`, heartbeat.AgentID, heartbeat.BootID, heartbeat.UptimeSeconds,
		heartbeat.ResticVersion, heartbeat.AcceptedRevision,
		heartbeat.StateFreeBytes, heartbeat.ClockOffsetMS,
		heartbeat.ErrorCode, heartbeat.ReceivedAt)
	if err != nil {
		return domain.Agent{}, err
	}
	if tag.RowsAffected() != 1 {
		return domain.Agent{}, domain.ErrAgentRevoked
	}
	return s.Agent(ctx, heartbeat.AgentID)
}

func (s *Store) RecordAgentInventory(ctx context.Context, inventory domain.AgentInventory) error {
	availableBytes, err := json.Marshal(inventory.AvailableBytes)
	if err != nil {
		return err
	}
	capabilities, err := json.Marshal(inventory.Capabilities)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		insert into agent_inventories (
			id, agent_id, captured_at, kernel, os_release, cpu_arch,
			agent_version, restic_version, containerized, available_bytes,
			clock_offset_ms, capabilities, created_at
		) select
			$1, id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
		from agents where id = $2 and status = 'ACTIVE'
	`, inventory.ID, inventory.AgentID, inventory.CapturedAt, inventory.Kernel,
		inventory.OSRelease, inventory.CPUArch, inventory.AgentVersion,
		inventory.ResticVersion, inventory.Containerized, availableBytes,
		inventory.ClockOffsetMS, capabilities, time.Now().UTC())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrAgentRevoked
	}
	return nil
}

func (s *Store) LatestAgentInventory(
	ctx context.Context,
	hostID uuid.UUID,
) (domain.AgentInventory, error) {
	var inventory domain.AgentInventory
	var availableBytes, capabilities []byte
	err := s.pool.QueryRow(ctx, `
		select i.id, i.agent_id, i.captured_at, i.kernel, i.os_release,
		       i.cpu_arch, i.agent_version, i.restic_version, i.containerized,
		       i.available_bytes, i.clock_offset_ms, i.capabilities
		from agent_inventories i
		join agents a on a.id = i.agent_id
		where a.host_id = $1
		order by i.captured_at desc, i.id desc
		limit 1
	`, hostID).Scan(
		&inventory.ID, &inventory.AgentID, &inventory.CapturedAt,
		&inventory.Kernel, &inventory.OSRelease, &inventory.CPUArch,
		&inventory.AgentVersion, &inventory.ResticVersion,
		&inventory.Containerized, &availableBytes, &inventory.ClockOffsetMS,
		&capabilities,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AgentInventory{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.AgentInventory{}, err
	}
	if err := json.Unmarshal(availableBytes, &inventory.AvailableBytes); err != nil {
		return domain.AgentInventory{}, err
	}
	if err := json.Unmarshal(capabilities, &inventory.Capabilities); err != nil {
		return domain.AgentInventory{}, err
	}
	return inventory, nil
}

func (s *Store) AcceptAgentConfig(
	ctx context.Context,
	agentID uuid.UUID,
	result domain.AgentConfigResult,
	at time.Time,
) error {
	tag, err := s.pool.Exec(ctx, `
		update agents a
		set accepted_revision = greatest(a.accepted_revision, $2),
		    config_error_code = case when a.desired_revision = $2 then '' else a.config_error_code end,
		    config_error_field = case when a.desired_revision = $2 then '' else a.config_error_field end,
		    updated_at = $4
		where a.id = $1 and a.status = 'ACTIVE'
		  and $2 <= a.desired_revision
		  and exists (
		    select 1 from agent_desired_states d
		    where d.agent_id = a.id and d.revision = $2 and d.config_hash = $3
		  )
	`, agentID, result.Revision, result.ConfigHash, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return domain.ErrNotFound
	}
	return nil
}

func (s *Store) RejectAgentConfig(
	ctx context.Context,
	agentID uuid.UUID,
	result domain.AgentConfigResult,
	at time.Time,
) error {
	_, err := s.pool.Exec(ctx, `
		update agents
		set config_error_code = $3, config_error_field = $4, updated_at = $5
		where id = $1 and status = 'ACTIVE' and desired_revision = $2
	`, agentID, result.Revision, result.ErrorCode, result.FieldPath, at)
	return err
}
