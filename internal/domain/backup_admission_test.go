package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBackupAdmissionRequestBounds(t *testing.T) {
	id := uuid.MustParse("019abcde-1234-7000-8000-000000000001")
	valid := BackupAdmissionRequest{ID: id, Owner: id, AgentID: id, DeliveryID: id, ConfigurationHash: strings.Repeat("a", 64), Lifetime: time.Hour}
	if valid.Validate() != nil {
		t.Fatal("valid request rejected")
	}
	for _, change := range []func(*BackupAdmissionRequest){
		func(r *BackupAdmissionRequest) { r.ID = uuid.Nil }, func(r *BackupAdmissionRequest) { r.Owner = uuid.New() },
		func(r *BackupAdmissionRequest) { r.AgentID = uuid.Nil }, func(r *BackupAdmissionRequest) { r.DeliveryID = uuid.Nil },
		func(r *BackupAdmissionRequest) { r.ConfigurationHash = strings.Repeat("A", 64) }, func(r *BackupAdmissionRequest) { r.ConfigurationHash = "secret-canary" },
		func(r *BackupAdmissionRequest) { r.Lifetime = 0 }, func(r *BackupAdmissionRequest) { r.Lifetime = 25 * time.Hour },
		func(r *BackupAdmissionRequest) { r.Lifetime = time.Minute + time.Nanosecond },
	} {
		r := valid
		change(&r)
		if r.Validate() != ErrBackupAdmission {
			t.Fatal("invalid admission request accepted")
		}
	}
}
