package domain

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGatewayDecisionRequestBounds(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	r := GatewayDecisionRequest{ID: id, AdmissionID: id, Owner: id, RuntimeID: id, Lifetime: 12 * time.Hour}
	if r.Validate() != nil {
		t.Fatal("valid request rejected")
	}
	for _, mutate := range []func(*GatewayDecisionRequest){
		func(r *GatewayDecisionRequest) { r.ID = uuid.Nil }, func(r *GatewayDecisionRequest) { r.AdmissionID = uuid.New() },
		func(r *GatewayDecisionRequest) { r.Owner = uuid.Nil }, func(r *GatewayDecisionRequest) { r.RuntimeID = uuid.Nil },
		func(r *GatewayDecisionRequest) { r.ExpectedRevision = -1 }, func(r *GatewayDecisionRequest) { r.ExpectedRevision = math.MaxInt64 },
		func(r *GatewayDecisionRequest) { r.Lifetime = 0 }, func(r *GatewayDecisionRequest) { r.Lifetime += time.Second },
		func(r *GatewayDecisionRequest) { r.Lifetime = time.Second + time.Nanosecond }, func(r *GatewayDecisionRequest) { r.Revoke = true },
	} {
		bad := r
		mutate(&bad)
		if bad.Validate() != ErrGatewayDecision {
			t.Fatal("unsafe request accepted")
		}
	}
	r.Revoke = true
	r.Lifetime = 0
	if r.Validate() != nil {
		t.Fatal("explicit withdrawal rejected")
	}
}
