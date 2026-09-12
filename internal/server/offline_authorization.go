package server

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
)

// OfflineAuthStore extends Store with offline authorization operations.
type OfflineAuthStore interface {
	IssueOfflineAuthorization(context.Context, domain.OfflineAuthorizationRequest) (domain.OfflineAuthorization, error)
	RenewOfflineAuthorization(context.Context, domain.OfflineRenewalRequest) (domain.OfflineAuthorization, error)
	RevokeOfflineAuthorization(context.Context, uuid.UUID, uuid.UUID) error
	DisableOfflineAuthorization(context.Context, uuid.UUID, uuid.UUID) error
	VerifyOfflineAuthorization(context.Context, uuid.UUID, uuid.UUID, int64) (domain.OfflineAuthorization, error)
	OfflineAuthorizationForGateway(context.Context, uuid.UUID) (domain.OfflineAuthorization, error)
	SubmitWritebackRecord(context.Context, domain.WritebackSubmission) (domain.WritebackRecord, error)
	ConfirmWritebackRecord(context.Context, uuid.UUID) error
	PendingWritebackRecords(context.Context, uuid.UUID, int) ([]domain.WritebackRecord, error)
}

func (c *ControlPlane) IssueOfflineAuthorization(ctx context.Context, agentID uuid.UUID, gatewayInstanceID uuid.UUID, lifetime time.Duration) (domain.OfflineAuthorization, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if c.gatewayPublicURL == "" || len(c.masterKey) != 32 {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}

	agent, err := c.store.Agent(ctx, agentID)
	if err != nil {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if agent.Status != "ACTIVE" {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}

	credentialHash := c.credentialConfigurationHash()
	prepared, err := c.store.PrepareAgentCredential(ctx, agentID, credentialHash, false)
	if err != nil {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	if prepared.AcceptedAt == nil {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}

	id, err := uuid.NewV7()
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}
	owner, err := uuid.NewV7()
	if err != nil {
		return domain.OfflineAuthorization{}, err
	}

	request := domain.OfflineAuthorizationRequest{
		ID:                id,
		Owner:             owner,
		GatewayInstanceID: gatewayInstanceID,
		AgentID:           agentID,
		DeliveryID:        prepared.ID,
		ConfigurationHash: credentialHash,
		Lifetime:          lifetime,
	}

	return store.IssueOfflineAuthorization(ctx, request)
}

func (c *ControlPlane) RenewOfflineAuthorization(ctx context.Context, authorizationID, owner uuid.UUID, currentSequence int64, newLifetime time.Duration) (domain.OfflineAuthorization, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}

	hash := c.credentialConfigurationHash()
	now := c.clock().UTC()

	request := domain.OfflineRenewalRequest{
		AuthorizationID:   authorizationID,
		Owner:             owner,
		CurrentSequence:   currentSequence,
		NewExpiresAt:      now.Add(newLifetime),
		ConfigurationHash: hash,
	}

	return store.RenewOfflineAuthorization(ctx, request)
}

func (c *ControlPlane) VerifyOfflineAuthorization(ctx context.Context, id, owner uuid.UUID, minSequence int64) (domain.OfflineAuthorization, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	return store.VerifyOfflineAuthorization(ctx, id, owner, minSequence)
}

func (c *ControlPlane) RecoverOfflineAuthorization(ctx context.Context, gatewayInstanceID uuid.UUID) (domain.OfflineAuthorization, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.OfflineAuthorization{}, domain.ErrOfflineAuthorization
	}
	return store.OfflineAuthorizationForGateway(ctx, gatewayInstanceID)
}

func (c *ControlPlane) SubmitWritebackRecord(ctx context.Context, submission domain.WritebackSubmission) (domain.WritebackRecord, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.WritebackRecord{}, domain.ErrWritebackUnavailable
	}
	return store.SubmitWritebackRecord(ctx, submission)
}

func (c *ControlPlane) ConfirmWritebackRecords(ctx context.Context, recordIDs []uuid.UUID) error {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.ErrWritebackUnavailable
	}
	for _, id := range recordIDs {
		if err := store.ConfirmWritebackRecord(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (c *ControlPlane) PendingWritebackRecords(ctx context.Context, gatewayInstanceID uuid.UUID, limit int) ([]domain.WritebackRecord, error) {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return nil, domain.ErrWritebackUnavailable
	}
	return store.PendingWritebackRecords(ctx, gatewayInstanceID, limit)
}

func (c *ControlPlane) RevokeOfflineAuthorization(ctx context.Context, id, owner uuid.UUID) error {
	store, ok := c.store.(OfflineAuthStore)
	if !ok {
		return domain.ErrOfflineAuthorization
	}
	return store.RevokeOfflineAuthorization(ctx, id, owner)
}
