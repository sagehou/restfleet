package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// RepositoryGatewayObservations projects existing authenticated replay/audit
// history, never caller-supplied status or a material/RFGA receipt. One bounded
// query covers the list page. Old observations remain readable after withdrawal
// but cannot describe a current authorization or a different delivery/config.
func (s *Store) RepositoryGatewayObservations(ctx context.Context, ids []uuid.UUID, configurationHash string) (map[uuid.UUID]domain.RepositoryGatewayObservation, error) {
	if len(ids) == 0 || len(ids) > 201 {
		return nil, domain.ErrRepositoryUnavailable
	}
	rows, err := s.pool.Query(ctx, `select distinct on (a.repository_id) a.repository_id,e.occurred_at,
		coalesce(a.released_at is null and a.expires_at>clock_timestamp() and o.closed_at is null
		and not g.revoked and g.runtime_id=a_runtime.runtime_id and g.issued_at<=clock_timestamp() and g.expires_at>clock_timestamp()
		and ag.status='ACTIVE' and ag.host_id=a.host_id and h.status='ACTIVE' and h.archived_at is null
		and c.status<>'DISABLED' and r.status in ('PROVISIONING','READY','DEGRADED')
		and r.archived_at is null and r.initialized_at is not null
		and a.host_id=r.host_id and a.gateway_id=r.gateway_username and a.storage_credential_id=r.storage_credential_id
		and a.gateway_secret_ref=r.gateway_secret_ref and a.restic_secret_ref=r.restic_secret_ref
		and d.id=a.delivery_id and d.repository_id=r.id and d.accepted_at is not null
		and d.gateway_secret_ref=r.gateway_secret_ref and d.restic_secret_ref=r.restic_secret_ref
		and a.configuration_hash=$2 and d.configuration_hash=$2,false)
		from gateway_backup_admissions a
		join gateway_pending_origins o on o.admission_id=a.id
		join lateral (select p.runtime_id,p.sequence,p.record_id from gateway_pending_records p
			join audit_events proof on proof.id=p.record_id and proof.action='GATEWAY_BACKEND_READY'
			join gateway_authorization_decisions issued on issued.admission_id=p.admission_id and issued.runtime_id=p.runtime_id
				and issued.revision=p.authorization_revision and not issued.revoked
				and p.occurred_at>=issued.issued_at and p.occurred_at<issued.expires_at
			where p.admission_id=a.id and p.runtime_id=o.runtime_id and p.kind='audit'
			order by p.sequence desc limit 1) a_runtime on true
		join audit_events e on e.id=a_runtime.record_id
		join repositories r on r.id=a.repository_id
		join agents ag on ag.id=a.agent_id
		join hosts h on h.id=a.host_id
		join storage_credentials c on c.id=a.storage_credential_id
		left join repository_agent_deliveries d on d.agent_id=a.agent_id
		left join lateral (select runtime_id,issued_at,expires_at,revoked from gateway_authorization_decisions
			where admission_id=a.id order by revision desc limit 1) g on true
		where a.repository_id=any($1::uuid[])
		order by a.repository_id,e.occurred_at desc,a.created_at desc,a_runtime.sequence desc`, ids, configurationHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := make(map[uuid.UUID]domain.RepositoryGatewayObservation, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var observed time.Time
		var authorized bool
		if err := rows.Scan(&id, &observed, &authorized); err != nil {
			return nil, err
		}
		observations[id] = domain.RepositoryGatewayObservation{VerifiedAt: observed.UTC(), Authorized: authorized}
	}
	return observations, rows.Err()
}
