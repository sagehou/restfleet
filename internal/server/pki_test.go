package server

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sagehou/restfleet/internal/domain"
)

type memoryCAStore struct {
	mu     sync.Mutex
	record *domain.AgentCARecord
}

func (s *memoryCAStore) AgentCA(context.Context) (domain.AgentCARecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		return domain.AgentCARecord{}, domain.ErrNotFound
	}
	return *s.record, nil
}

func (s *memoryCAStore) InitializeAgentCA(
	_ context.Context,
	record domain.AgentCARecord,
) (domain.AgentCARecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record == nil {
		s.record = &record
	}
	return *s.record, nil
}

func TestLoadOrCreateAgentCAStoresOnlyEnvelopeCiphertext(t *testing.T) {
	store := &memoryCAStore{}
	masterKey := bytes.Repeat([]byte{9}, 32)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	first, err := LoadOrCreateAgentCA(context.Background(), store, masterKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if store.record == nil || bytes.Contains(store.record.PrivateKey.Ciphertext, []byte("PRIVATE KEY")) ||
		len(store.record.PrivateKey.WrappedDataKey) == 0 {
		t.Fatal("CA private key was not stored as an envelope")
	}
	second, err := LoadOrCreateAgentCA(context.Background(), store, masterKey, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.CertificatePEM(), second.CertificatePEM()) {
		t.Fatal("existing CA was not reused")
	}
	if _, err := LoadOrCreateAgentCA(context.Background(), store, bytes.Repeat([]byte{8}, 32), now); err == nil {
		t.Fatal("wrong master key decrypted the CA")
	}
}
