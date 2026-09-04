package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

type AgentCAStore interface {
	AgentCA(context.Context) (domain.AgentCARecord, error)
	InitializeAgentCA(context.Context, domain.AgentCARecord) (domain.AgentCARecord, error)
}

func LoadOrCreateAgentCA(
	ctx context.Context,
	store AgentCAStore,
	masterKey []byte,
	now time.Time,
) (*security.AgentCA, error) {
	if len(masterKey) != 32 {
		return nil, errors.New("master key must be 32 decoded bytes")
	}
	record, err := store.AgentCA(ctx)
	if errors.Is(err, domain.ErrNotFound) {
		ca, privateKeyPEM, createErr := security.NewAgentCA(now)
		if createErr != nil {
			return nil, createErr
		}
		defer clear(privateKeyPEM)
		secretID, createErr := uuid.NewV7()
		if createErr != nil {
			return nil, createErr
		}
		aad := []byte("restfleet:agent-ca-private-key:v1:" + secretID.String())
		sealed, createErr := security.SealEnvelope(masterKey, privateKeyPEM, aad)
		if createErr != nil {
			return nil, createErr
		}
		record = domain.AgentCARecord{
			CertificatePEM: ca.CertificatePEM(),
			CreatedAt:      now.UTC(),
			PrivateKey: domain.SecretEnvelope{
				ID: secretID, Kind: "AGENT_CA_PRIVATE_KEY",
				Algorithm: security.EnvelopeAlgorithm, KeyID: "master:v1",
				Ciphertext: sealed.Ciphertext, Nonce: sealed.Nonce,
				WrappedDataKey: sealed.WrappedDataKey, WrapNonce: sealed.WrapNonce,
				AAD: sealed.AAD, CreatedAt: now.UTC(),
			},
		}
		record, err = store.InitializeAgentCA(ctx, record)
	}
	if err != nil {
		return nil, fmt.Errorf("load agent CA: %w", err)
	}
	if record.PrivateKey.Kind != "AGENT_CA_PRIVATE_KEY" ||
		record.PrivateKey.Algorithm != security.EnvelopeAlgorithm {
		return nil, errors.New("unsupported agent CA secret metadata")
	}
	privateKeyPEM, err := security.OpenEnvelope(masterKey, security.Envelope{
		Ciphertext:     record.PrivateKey.Ciphertext,
		Nonce:          record.PrivateKey.Nonce,
		WrappedDataKey: record.PrivateKey.WrappedDataKey,
		WrapNonce:      record.PrivateKey.WrapNonce,
		AAD:            record.PrivateKey.AAD,
	})
	if err != nil {
		return nil, fmt.Errorf("decrypt agent CA: %w", err)
	}
	defer clear(privateKeyPEM)
	ca, err := security.LoadAgentCA(record.CertificatePEM, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse agent CA: %w", err)
	}
	return ca, nil
}
