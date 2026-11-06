package server

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

type indexProjectionStore struct {
	Store
	repositories []domain.Repository
	observed     map[uuid.UUID]domain.RepositoryGatewayObservation
	indexed      []uuid.UUID
	pages        int
}

func (s *indexProjectionStore) Repositories(_ context.Context, after uuid.UUID, limit int) ([]domain.Repository, error) {
	s.pages++
	start := 0
	for start < len(s.repositories) && s.repositories[start].ID.String() <= after.String() {
		start++
	}
	end := start + limit
	if end > len(s.repositories) {
		end = len(s.repositories)
	}
	return append([]domain.Repository(nil), s.repositories[start:end]...), nil
}
func (s *indexProjectionStore) RepositoryGatewayObservations(context.Context, []uuid.UUID, string) (map[uuid.UUID]domain.RepositoryGatewayObservation, error) {
	return s.observed, nil
}
func (s *indexProjectionStore) EnqueueSnapshotIndex(_ context.Context, o domain.Operation, _ string) (domain.Operation, error) {
	s.indexed = append(s.indexed, *o.RepositoryID)
	return o, nil
}

func TestSnapshotIndexerUsesReadyProjectionAndVisitsEveryPage(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	store := &indexProjectionStore{observed: make(map[uuid.UUID]domain.RepositoryGatewayObservation)}
	for i := 0; i < 405; i++ {
		id := uuid.Must(uuid.NewV7())
		store.repositories = append(store.repositories, domain.Repository{ID: id, Status: "PROVISIONING", InitializedAt: &now, AgentCredentialAcceptedAt: &now})
		store.observed[id] = domain.RepositoryGatewayObservation{VerifiedAt: now, Authorized: true}
	}
	// A stale persisted READY flag must not enqueue a repository.
	store.repositories[10].Status = "READY"
	delete(store.observed, store.repositories[10].ID)
	c := &ControlPlane{store: store, clock: func() time.Time { return now }, gatewayPublicURL: "https://gateway.example"}
	if err := c.indexReadyRepositories(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.pages != 3 || len(store.indexed) != 404 || store.indexed[len(store.indexed)-1] != store.repositories[404].ID {
		t.Fatal("indexer ignored READY projection or starved later pages")
	}
	for _, id := range store.indexed {
		if id == store.repositories[10].ID {
			t.Fatal("stale READY was indexed")
		}
	}
}

func TestSnapshotPlanMappingRejectsConflictingAndMalformedTags(t *testing.T){
 first,second:=uuid.Must(uuid.NewV7()),uuid.Must(uuid.NewV7())
 for _,tags:=range [][]string{
  {"restfleet:managed","restfleet:plan:"+first.String(),"restfleet:plan:"+second.String(),"restfleet:plan:"+first.String()},
  {"restfleet:managed","restfleet:plan:invalid","restfleet:plan:"+first.String()},
  {"restfleet:plan:"+first.String()},
 }{if resolveSnapshotPlan(tags)!=nil{t.Fatal("untrusted snapshot became managed")}}
 if got:=resolveSnapshotPlan([]string{"restfleet:managed","restfleet:plan:"+first.String()});got==nil || *got!=first{t.Fatal("valid managed snapshot lost mapping")}
}
