package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/rclone"
	"github.com/sagehou/restfleet/internal/security"
)

type serviceRepository struct {
	config   ServiceRepository
	source   ed25519.PrivateKey
	receiver *gatewaypending.MaterialReceiver
	material *net.UnixListener
	authority *net.UnixListener
	owner    *AuthorizedBackup
	queue    *gatewaypending.Queue
}

// Service owns one runtime/supervisor, global audit producer, TLS transport and
// all configured repository owners/channels. WithBackup is a trusted in-process
// seam; Agent session admission/delivery and a deployable command still need
// their protocol. Never expose a config/run callback to an Agent request.
type Service struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	workers      sync.WaitGroup
	mu           sync.Mutex
	failure      error
	config       ServiceConfig
	runtime      *rclone.Runtime
	supervisor   *Supervisor
	public       *PublicServer
	listener     net.Listener
	globalQueue  *gatewaypending.Queue
	global       *GlobalAuditRecorder
	repositories []*serviceRepository
}

// StartService consumes fresh encrypted queues and performs each one-shot
// initialization concurrently. Any partial failure cancels/joins all owners and
// preserves queues/fences. Public TLS binds only after every receipt succeeded.
// ctx owns the complete lifetime; Close joins shutdown. It never takes over an
// existing source, registers centrally, renews authority or releases admission.
func StartService(ctx context.Context, config ServiceConfig) (_ *Service, failure error) {
	config.RecipientPublic = bytes.Clone(config.RecipientPublic)
	config.Repositories = append([]ServiceRepository(nil), config.Repositories...)
	if config.Validate() != nil || ctx.Err() != nil || !serviceDurableDirectory(config.AuditQueueDirectory) {
		return nil, ErrService
	}
	for _, r := range config.Repositories {
		if !serviceDurableDirectory(r.QueueDirectory) {
			return nil, ErrService
		}
	}
	work, cancel := context.WithCancel(ctx)
	s := &Service{ctx: work, cancel: cancel, done: make(chan struct{}), config: config}
	defer func() {
		if failure != nil {
			cancel()
			s.cleanup()
		}
	}()
	auditSource, central, err := security.LoadGatewayTrust(config.AuditSourceFile, config.CentralPinFile)
	if err != nil {
		return nil, ErrService
	}
	defer clear(auditSource)
	keys := map[string]bool{string(auditSource[32:]): true}
	for _, c := range config.Repositories {
		r := &serviceRepository{config: c}
		s.repositories = append(s.repositories, r)
		var pin ed25519.PublicKey
		r.source, pin, err = security.LoadGatewayTrust(c.SourceFile, config.CentralPinFile)
		if err != nil || !bytes.Equal(pin, central) || keys[string(r.source[32:])] {
			return nil, ErrService
		}
		keys[string(r.source[32:])] = true
		r.receiver, err = gatewaypending.NewMaterialReceiver(c.Binding, r.source, central)
		if err != nil {
			return nil, ErrService
		}
	}
	s.runtime, err = rclone.NewRuntime(config.RuntimeDirectory, config.RcloneBinary)
	if err != nil {
		return nil, ErrService
	}
	limits := gatewaypending.Limits{MaxBytes: config.MaxBytes, MaxRecords: config.MaxRecords}
	s.globalQueue, err = gatewaypending.CreateGlobalAudit(config.AuditQueueDirectory, config.AuditOrigin, [32]byte(config.RecipientPublic), auditSource, central, limits)
	if err != nil {
		return nil, ErrService
	}
	s.global, err = NewGlobalAuditRecorder(s.globalQueue, config.AuditOrigin, auditSource.Public().(ed25519.PublicKey), central)
	if err != nil {
		return nil, ErrService
	}
	s.supervisor, err = NewSupervisor(s.runtime, config.MaxSessions, s.global.Record, s.global.Ready)
	if err != nil {
		return nil, ErrService
	}
	cert, err := security.ReadProtectedGatewayFile(config.CertificateFile, 32<<10)
	if err != nil {
		return nil, ErrService
	}
	defer clear(cert)
	key, err := security.ReadProtectedGatewayFile(config.TLSKeyFile, 32<<10)
	if err != nil {
		return nil, ErrService
	}
	defer clear(key)
	s.public, err = NewPublicServer(s.supervisor, cert, key)
	if err != nil {
		return nil, ErrService
	}
	for _, r := range s.repositories {
		r.material, err = gatewaypending.ListenReplay(r.config.MaterialSocket, config.SharedGroup)
		if err != nil {
			return nil, ErrService
		}
	}
	initialized := make(chan error, len(s.repositories))
	for _, r := range s.repositories {
		go func() {
			initialized <- r.receiver.ReceiveWaiting(work, r.material, config.ServerUID, time.Duration(config.StartupWaitSeconds)*time.Second,
				func(install context.Context, material security.GatewayMaterial) (func(), error) {
					if install.Err() != nil || !bytes.Equal(material.PendingRecipient[:], config.RecipientPublic) {
						return nil, ErrService
					}
					var err error
					r.owner, r.queue, err = InstallGatewayMaterial(s.supervisor, r.config.QueueDirectory, limits, material, r.source, central)
					if err != nil {
						return nil, ErrService
					}
					return r.owner.Close, nil
				}, s.denied("material_rejected"), config.SharedGroup)
		}()
	}
	var initializationFailed bool
	for range s.repositories {
		if <-initialized != nil {
			initializationFailed = true
			cancel()
		}
	}
	if initializationFailed || work.Err() != nil || !s.global.Ready() {
		return nil, ErrService
	}
	for _, r := range s.repositories {
		if !r.owner.ready() {
			return nil, ErrService
		}
		r.authority, err = gatewaypending.ListenReplay(r.config.AuthoritySocket, config.SharedGroup)
		if err != nil {
			return nil, ErrService
		}
	}
	s.listener, err = net.Listen("tcp", config.ListenAddress)
	if err != nil || work.Err() != nil {
		return nil, ErrService
	}
	for _, r := range s.repositories {
		s.workers.Go(func() {
			if err := gatewaypending.ServeAuthorization(work, r.authority, config.ServerUID, r.config.Binding, r.source, central,
				r.owner.AcceptAuthorization, s.denied("authority_rejected"), config.SharedGroup); err != nil && work.Err() == nil {
				s.fail()
			}
		})
		s.workers.Go(func() {
			select {
			case <-r.owner.watchDone:
				if work.Err() == nil {
					s.fail()
				}
			case <-work.Done():
			}
		})
	}
	s.workers.Go(func() {
		if err := s.public.Serve(work, s.listener); err != nil && work.Err() == nil {
			s.fail()
		}
	})
	s.workers.Go(s.replay)
	go func() {
		<-work.Done()
		s.cleanup()
		close(s.done)
	}()
	return s, nil
}

func (s *Service) denied(reason string) func(context.Context) error {
	return func(ctx context.Context) error {
		return s.global.Record(ctx, Event{Action: "channel_denied", Reason: reason})
	}
}

func (s *Service) fail() {
	s.mu.Lock()
	s.failure = ErrService
	s.mu.Unlock()
	s.cancel()
}

// One wire per queue per sweep bounds memory and prevents a busy producer from
// starving other queues. Connection/receipt loss preserves exact original wire
// and retries on the next sweep, without changing authorization or revisions.
func (s *Service) replay() {
	queues := []*gatewaypending.Queue{s.globalQueue}
	for _, r := range s.repositories {
		queues = append(queues, r.queue)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for _, q := range queues {
			if s.ctx.Err() != nil {
				return
			}
			wire, err := q.Next()
			if err != nil {
				s.fail()
				return
			}
			if wire == nil {
				continue
			}
			ack, err := gatewaypending.Replay(s.ctx, s.config.ReplaySocket, s.config.ServerUID, s.config.AuditOrigin.RuntimeID, wire, s.config.SharedGroup)
			if err == nil && q.Acknowledge(ack) != nil {
				s.fail()
				return
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// The service context is joined with the trusted operation context. The same
// immutable owner is reused; callbacks borrow capabilities until return only.
func (s *Service) WithBackup(ctx context.Context, repository, operation uuid.UUID, run func(context.Context, Access) error) error {
	if s.ctx.Err() != nil {
		return ErrService
	}
	for _, r := range s.repositories {
		if r.config.Binding.RepositoryID == repository {
			work, cancel := context.WithCancel(ctx)
			defer cancel()
			stop := context.AfterFunc(s.ctx, cancel)
			defer stop()
			if r.owner.WithBackup(work, operation, run) != nil {
				return ErrService
			}
			return nil
		}
	}
	return ErrService
}

func (s *Service) Close() error {
	s.cancel()
	return s.Wait()
}

func (s *Service) Wait() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

// Ingress/session cancellation precedes joins; final channel denial/end audits
// finish while the global producer and all queues remain open. No central seal,
// queue deletion, admission release or runtime restart occurs during cleanup.
func (s *Service) cleanup() {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for _, r := range s.repositories {
		if r.receiver != nil {
			r.receiver.Close()
		}
		if r.material != nil {
			_ = r.material.Close()
		}
		if r.authority != nil {
			_ = r.authority.Close()
		}
	}
	if s.supervisor != nil {
		s.supervisor.Close()
	}
	for _, r := range s.repositories {
		if r.owner != nil {
			r.owner.Close()
		}
	}
	s.workers.Wait()
	for _, r := range s.repositories {
		clear(r.source)
		if r.queue != nil && r.queue.Close() != nil {
			s.fail()
		}
	}
	if s.global != nil {
		s.global.Close()
	}
	if s.globalQueue != nil && s.globalQueue.Close() != nil {
		s.fail()
	}
	if s.runtime != nil && s.runtime.Close() != nil {
		s.fail()
	}
}
