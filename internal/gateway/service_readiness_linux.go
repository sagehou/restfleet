package gateway

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Production observations use the original owner, material watcher, readonly
// config probe and signed queue. They never request central authority, init a
// repository or report a successful backup. One serial sweep bounds resources.
func (s *Service) observeRepositories() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		certificate := s.public.http.TLSConfig.Certificates[0].Leaf
		if certificate == nil || time.Now().Before(certificate.NotBefore) || !time.Now().Before(certificate.NotAfter) {
			s.fail()
			return
		}
		for _, r := range s.repositories {
			s.mu.Lock()
			if s.stopping || s.ctx.Err() != nil {
				s.mu.Unlock()
				return
			}
			active := false
			for _, backup := range s.sessions {
				active = active || (backup.repositoryID == r.config.Binding.RepositoryID && backup.state != "CLOSED" && backup.state != "FAILED")
			}
			r.probing = !active
			s.mu.Unlock()
			work, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			if active {
				// Failure supplies no fresh evidence. Original guards/watchdogs
				// still own cancellation; a probe cannot revive failed owners.
				_ = s.supervisor.observeActiveBackend(work, r.config.Binding.RepositoryID)
			} else if id, err := uuid.NewV7(); err == nil {
				_ = r.owner.withBackup(work, id, func(context.Context, Access) error { return nil }, true)
			}
			cancel()
			s.mu.Lock()
			r.probing = false
			s.mu.Unlock()
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
