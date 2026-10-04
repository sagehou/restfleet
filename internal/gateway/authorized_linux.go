package gateway

import (
	"context"
	"crypto/ed25519"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

// AuthorizedBackup owns ONE already admitted, registered and provisioned
// binding for its process lifetime. The trusted coordinator MUST deliver the
// origin, material and keys through an authenticated protected channel before
// constructing it. This internal seam does not provide that delivery, request
// admission, release a fence or claim crash recovery. Reuse the same owner and
// Authorization for sequential sessions; never reconstruct either to clear a
// failure, revocation, clock rollback or token revision.
type AuthorizedBackup struct {
	supervisor    *Supervisor
	authorization *Authorization
	queue         *gatewaypending.Queue
	source        ed25519.PublicKey
	expires       time.Time
	record        func(context.Context, Event) error
	persist       func(context.Context, []byte) error
	closeRecorder func()
	remote        string

	mu                   sync.Mutex
	closed, busy, failed bool
	cancel               context.CancelFunc
	running              sync.WaitGroup
	stopWatch            chan struct{}
	watchDone            chan struct{}
	stopOnce             sync.Once
	material             sync.Mutex
	raw                  []byte // Current token revision, never a Restic password.
}

func NewAuthorizedBackup(supervisor *Supervisor, authorization *Authorization, queue *gatewaypending.Queue,
	origin domain.GatewayPendingOrigin, raw []byte, remote string,
) (*AuthorizedBackup, error) {
	if supervisor == nil || authorization == nil || queue == nil || origin.ClosedAt != nil ||
		origin.Admission.ReleasedAt != nil || len(raw) == 0 || len(raw) > 256<<10 {
		return nil, ErrAuthorization
	}
	a := origin.Admission
	b := security.GatewayAuthorizationBinding{AdmissionID: a.ID, Owner: a.Owner, RuntimeID: origin.RuntimeID,
		AgentID: a.AgentID, HostID: a.HostID, RepositoryID: a.RepositoryID, GatewayID: a.GatewayID,
		StorageCredentialID: a.StorageCredentialID, DeliveryID: a.DeliveryID, GatewaySecretRef: a.GatewaySecretRef,
		ResticSecretRef: a.ResticSecretRef, ConfigurationHash: a.ConfigurationHash}
	if b != authorization.binding || !a.ExpiresAt.After(time.Now()) || authorization.Status() != AuthorizationValid ||
		queue.CheckProducer(b, origin.PublicKey, authorization.key) != nil || (supervisor.auditReady != nil && !supervisor.auditReady()) {
		return nil, ErrAuthorization
	}
	authorization.mu.Lock()
	grant := authorization.current
	authorization.mu.Unlock()
	if time.Unix(grant.ExpiresAt, 0).After(a.ExpiresAt) || time.Unix(grant.IssuedAt, 0).Before(a.CreatedAt.Truncate(time.Second)) {
		return nil, ErrAuthorization
	}
	record, err := NewPendingAuditRecorder(queue, authorization)
	if err != nil {
		return nil, ErrAuthorization
	}
	persist, closeRecorder, err := NewPendingRefreshRecorder(queue, authorization, raw, remote, origin.InitialSecretRevision)
	if err != nil {
		return nil, ErrAuthorization
	}
	if queue.ClaimProducer(b, origin.PublicKey, authorization.key) != nil {
		closeRecorder()
		return nil, ErrAuthorization
	}
	owner := &AuthorizedBackup{supervisor: supervisor, authorization: authorization, queue: queue,
		source: append(ed25519.PublicKey(nil), origin.PublicKey...), expires: a.ExpiresAt, record: record,
		persist: persist, closeRecorder: closeRecorder, raw: append([]byte(nil), raw...), remote: remote,
		stopWatch: make(chan struct{}), watchDone: make(chan struct{})}
	go owner.watch()
	return owner, nil
}

func (a *AuthorizedBackup) ready() bool {
	return a.authorization.Status() == AuthorizationValid && time.Now().Before(a.expires) &&
		a.queue.CheckProducer(a.authorization.binding, a.source, a.authorization.key) == nil &&
		(a.supervisor.auditReady == nil || a.supervisor.auditReady())
}

// AcceptAuthorization updates this existing owner's state only; it cannot
// reconstruct an expired/failed owner or extend the original admission. Wire
// must arrive through the pinned local authority channel, never an Agent API.
// Receipt of a revocation is not a cleanup acknowledgement: the lifetime
// watchdog/each operation guard cancel and join work independently.
func (a *AuthorizedBackup) AcceptAuthorization(ctx context.Context, wire []byte) error {
	statement, err := security.VerifyGatewayStatement(wire, a.authorization.key)
	if err != nil || statement.Binding != a.authorization.binding || ctx.Err() != nil ||
		(!statement.Revoked && time.Unix(statement.ExpiresAt, 0).After(a.expires)) {
		return ErrAuthorization
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.failed {
		return ErrAuthorization
	}
	if !statement.Revoked && !a.ready() {
		a.failed = true
		a.stopOnce.Do(func() { close(a.stopWatch) })
		return ErrAuthorization
	}
	if a.authorization.Accept(wire) != nil || ctx.Err() != nil {
		return ErrAuthorization
	}
	return nil
}

// One lifetime watchdog covers idle owners AND active sessions. It first gates
// new work and cancels the current session, then joins before clearing material.
// Queue/fence recovery remains the caller's responsibility after it stops.
func (a *AuthorizedBackup) watch() {
	defer close(a.watchDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
watching:
	for {
		select {
		case <-a.stopWatch:
			break watching
		case <-ticker.C:
			if !a.ready() {
				break watching
			}
		}
	}
	a.mu.Lock()
	a.failed = true
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	a.running.Wait()
	a.clearMaterial()
}

// WithBackup creates a fresh session capability using trusted local operation
// identity. No DB access is needed while the accepted grant and queue are valid.
// Every request and backend operation checks local authority; the watchdog also
// cancels idle/blocked work. Returning joins the runtime, subprocesses, requests,
// token watcher and end audit. Any failure permanently blocks this owner. Even a
// successful return neither freezes/drains the source nor releases its fence.
func (a *AuthorizedBackup) WithBackup(ctx context.Context, operation uuid.UUID, run func(context.Context, Access) error) (result error) {
	if operation.Version() != 7 || operation.Variant() != uuid.RFC4122 || run == nil || ctx.Err() != nil {
		return ErrAuthorization
	}
	a.mu.Lock()
	if a.closed || a.failed || a.busy {
		a.mu.Unlock()
		return ErrAuthorization
	}
	if !a.ready() {
		a.failed = true
		a.mu.Unlock()
		a.stopOnce.Do(func() { close(a.stopWatch) })
		<-a.watchDone
		return ErrAuthorization
	}
	work, cancel := context.WithDeadline(ctx, a.expires)
	a.busy, a.cancel = true, cancel
	a.running.Add(1)
	a.mu.Unlock()
	defer func() {
		usable := work.Err() == nil && a.ready()
		cancel()
		if result == nil && !usable {
			result = ErrAuthorization
		}
		a.mu.Lock()
		a.busy, a.cancel = false, nil
		a.failed = a.failed || result != nil
		a.mu.Unlock()
		a.running.Done()
		if result != nil {
			a.stopOnce.Do(func() { close(a.stopWatch) })
			<-a.watchDone
		}
	}()
	b := a.authorization.binding
	a.material.Lock()
	raw := append([]byte(nil), a.raw...)
	a.material.Unlock()
	defer clear(raw)
	request := BackupRequest{Binding: Binding{HostID: b.HostID, RepositoryID: b.RepositoryID, GatewayID: b.GatewayID, OperationID: operation},
		CredentialID: b.StorageCredentialID, Config: raw, Remote: a.remote}
	persist := func(ctx context.Context, next []byte) error {
		a.material.Lock()
		defer a.material.Unlock()
		if err := a.persist(ctx, next); err != nil {
			cancel()
			return err
		}
		clear(a.raw)
		a.raw = append([]byte(nil), next...)
		return nil
	}
	return a.supervisor.withBackup(work, request, persist, run, nil, a)
}

// Close cancels and joins all owned work, clears plaintext and freezes append.
// The queue remains caller-owned for verified replay. Close is not a central
// cleanup receipt: failures or an undrained queue still retain the fence.
func (a *AuthorizedBackup) Close() {
	a.mu.Lock()
	a.closed = true
	if a.cancel != nil {
		a.cancel()
	}
	a.mu.Unlock()
	a.stopOnce.Do(func() { close(a.stopWatch) })
	<-a.watchDone
}

// Called only after all work joined, including every final recorder callback.
func (a *AuthorizedBackup) clearMaterial() {
	a.material.Lock()
	defer a.material.Unlock()
	a.closeRecorder()
	clear(a.raw)
	a.raw = nil
	a.queue.Freeze()
}
