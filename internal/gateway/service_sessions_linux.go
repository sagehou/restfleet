package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

// These records own live routes and bounded request deduplication, not jobs or
// backup results. They survive a lost HTTP response, never a Gateway restart.
type serviceBackup struct {
	agentID, repositoryID uuid.UUID
 reader *domain.GatewayReadBinding
	requestedDeadline     time.Time
	sessionID             uuid.UUID
	expires               time.Time
	state, endpoint       string // Under Service.mu.
	ready, done, finish   chan struct{}
	finishOnce            sync.Once
}

type serviceBackupResponse struct {
	SessionID    uuid.UUID `json:"session_id"`
	State        string    `json:"state"`
	EndpointPath string    `json:"endpoint_path"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (s *Service) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/gateway/") {
		s.supervisor.ServeHTTP(w, r)
		return
	}
	id, authenticated := authenticatedAgent(r)
 reader,readAuthenticated:=authenticatedGatewayReader(r)
	if (!authenticated && !readAuthenticated) || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery ||
		r.URL.Fragment != "" || r.URL.User != nil || r.URL.Scheme != "" || r.URL.Host != "" || r.URL.Opaque != "" {
		s.rejectSession(w, r, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/gateway/v1/readiness" {
		known, ready := false, s.ctx.Err() == nil && s.global.Ready()
		for _, owner := range s.repositories {
			known = known || owner.config.Binding.AgentID == id
			ready = ready && owner.owner.ready()
		}
		if !authenticated || !known {
			s.rejectSession(w, r, http.StatusForbidden)
		} else if !ready {
			http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{\"ready\":true}\n")
		}
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 7 || parts[1] != "gateway" || parts[2] != "v1" || parts[3] != "repositories" || (parts[5] != "backup-sessions" && parts[5]!="read-sessions") {
		s.rejectSession(w, r, http.StatusForbidden)
		return
	}
	readOnly:=parts[5]=="read-sessions"
 if readOnly && (!readAuthenticated || reader.OperationID.String()!=parts[6] || reader.RepositoryID.String()!=parts[4]) || !readOnly && !authenticated {
  s.rejectSession(w,r,http.StatusForbidden);return
 }
 repository, validRepo := sessionRepositoryUUID(parts[4])
	operation, validOperation := sessionOperationUUID(parts[6])
	if !validRepo || !validOperation {
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	var owner *serviceRepository
	for _, candidate := range s.repositories {
		if candidate.config.Binding.RepositoryID == repository && ((!readOnly && candidate.config.Binding.AgentID == id) ||
 (readOnly && candidate.config.Binding.AdmissionID==reader.AdmissionID && candidate.config.Binding.RuntimeID==reader.RuntimeID)) {
			owner = candidate
			break
		}
	}
	if owner == nil || !owner.owner.ready() || s.ctx.Err() != nil {
		s.rejectSession(w, r, http.StatusForbidden)
		return
	}
 var scope *domain.GatewayReadBinding
 if readOnly {reader.ConfigurationHash=owner.config.Binding.ConfigurationHash;scope=&reader;id=owner.config.Binding.AgentID}
 if r.Method == http.MethodPost {
  s.openSession(w, r, owner, operation,scope)
		return
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodDelete) || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	backup := s.sessions[operation]
	s.mu.Unlock()
	if backup == nil {
		http.Error(w, "gateway session unavailable", http.StatusNotFound)
		return
	}
	if backup.agentID != id || backup.repositoryID != repository || (backup.reader==nil)!=(!readOnly) ||
 (readOnly && !sameGatewayReader(*backup.reader,reader)) {
		s.rejectSession(w, r, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		backup.finishOnce.Do(func() { close(backup.finish) })
		select {
		case <-backup.done:
			s.mu.Lock()
			closed := backup.state == "CLOSED"
			s.mu.Unlock()
			if closed {
				w.WriteHeader(http.StatusNoContent)
			} else {
				http.Error(w, "gateway cleanup unavailable", http.StatusServiceUnavailable)
			}
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			http.Error(w, "gateway cleanup unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	s.sessionResponse(w, backup, http.StatusOK)
}

func sessionRepositoryUUID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw && id.Version() == 7 && id.Variant() == uuid.RFC4122
}

func sessionOperationUUID(raw string) (uuid.UUID, bool) {
	id, err := uuid.Parse(raw)
	return id, err == nil && id.String() == raw && domain.ValidOperationID(id)
}

func (s *Service) rejectSession(w http.ResponseWriter, r *http.Request, status int) {
	if s.denied("session_rejected")(r.Context()) != nil {
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Error(w, "gateway session rejected", status)
}

func (s *Service) openSession(w http.ResponseWriter, r *http.Request, owner *serviceRepository, operation uuid.UUID,reader *domain.GatewayReadBinding) {
	if r.Header.Get("Content-Type") != "application/json" {
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	var request struct {
		Deadline time.Time `json:"deadline"`
	}
	if err != nil || json.Unmarshal(raw, &request) != nil {
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	canonical, err := json.Marshal(request)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) ||
		request.Deadline.Location() != time.UTC {
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if existing := s.sessions[operation]; existing != nil {
		conflict := existing.agentID != owner.config.Binding.AgentID || existing.repositoryID != owner.config.Binding.RepositoryID ||
			!existing.requestedDeadline.Equal(request.Deadline) || (existing.reader==nil)!=(reader==nil) ||
 (reader!=nil && !sameGatewayReader(*reader,*existing.reader))
		s.mu.Unlock()
		if conflict {
			s.rejectSession(w, r, http.StatusConflict)
		} else {
			s.sessionResponse(w, existing, http.StatusOK)
		}
		return
	}
	if !request.Deadline.After(time.Now()) {
		s.mu.Unlock()
		s.rejectSession(w, r, http.StatusBadRequest)
		return
	}
	if owner.probing {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "gateway repository busy", http.StatusConflict)
		return
	}
	if s.stopping || s.ctx.Err() != nil || len(s.sessions) >= 4096 {
		s.mu.Unlock()
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	for _, active := range s.sessions {
		if active.state != "CLOSED" && active.state != "FAILED" &&
			(active.repositoryID == owner.config.Binding.RepositoryID || active.agentID == owner.config.Binding.AgentID) {
			s.mu.Unlock()
   w.Header().Set("Retry-After","1")
   http.Error(w,"gateway repository busy",http.StatusConflict)
			return
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		s.mu.Unlock()
		http.Error(w, "gateway unavailable", http.StatusServiceUnavailable)
		return
	}
	deadline := time.Now().UTC().Add(24 * time.Hour)
 if reader!=nil{deadline=time.Now().UTC().Add(5*time.Minute)}
	if request.Deadline.Before(deadline) {
		deadline = request.Deadline
	}
 if reader!=nil && r.TLS.PeerCertificates[0].NotAfter.Before(deadline){deadline=r.TLS.PeerCertificates[0].NotAfter}
	if owner.owner.expires.Before(deadline) {
		deadline = owner.owner.expires
	}
	backup := &serviceBackup{agentID: owner.config.Binding.AgentID, repositoryID: owner.config.Binding.RepositoryID,
		requestedDeadline: request.Deadline, sessionID: id, expires: deadline, state: "STARTING",
		ready: make(chan struct{}), done: make(chan struct{}), finish: make(chan struct{}),reader:reader}
	s.sessions[operation] = backup
	// Add under the same gate cleanup closes before waiting for all work.
	s.workers.Go(func() {
		ctx, cancel := context.WithDeadline(s.ctx, deadline)
		defer cancel()
		// Lost requests do not cancel or restart a session. The same owner and
		// authority are used; only the Gateway-generated session ID scopes locks.
		run:=func(work context.Context, access Access) error {
			s.mu.Lock()
			backup.state, backup.endpoint = "ACTIVE", access.EndpointPath
			s.mu.Unlock()
			close(backup.ready)
			select {
			case <-backup.finish:
				return nil
			case <-work.Done():
				return work.Err()
			}
  }
  var err error
  if reader!=nil{err=owner.owner.WithRead(ctx,id,*reader,run)}else{err=owner.owner.WithBackup(ctx,id,run)}
  s.mu.Lock()
  backup.state, backup.endpoint = "CLOSED", ""
		if err != nil {
			backup.state = "FAILED"
		}
		s.mu.Unlock()
		close(backup.done)
	})
	s.mu.Unlock()
	select {
	case <-backup.ready:
	case <-backup.done:
	case <-r.Context().Done():
		return
	case <-time.After(20 * time.Second):
	}
	s.sessionResponse(w, backup, http.StatusAccepted)
}

func (s *Service) sessionResponse(w http.ResponseWriter, backup *serviceBackup, status int) {
	s.mu.Lock()
	response := serviceBackupResponse{backup.sessionID, backup.state, backup.endpoint, backup.expires}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
