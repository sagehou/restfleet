package server

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

var (
	ErrInvalidCredentials    = errors.New("invalid credentials")
	ErrInvalidBootstrapToken = errors.New("invalid bootstrap token")
	ErrUnauthorized          = errors.New("authentication required")
	ErrCSRF                  = errors.New("invalid CSRF token")
)

const (
	maxPasswordRunes = 1024
	maxPasswordBytes = 4 * maxPasswordRunes
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Store is the persistence port used by control-plane domain operations.
type Store interface {
	Ping(context.Context) error
	SchemaVersion(context.Context) (int, error)
	BootstrapRequired(context.Context) (bool, error)
	Bootstrap(context.Context, domain.User, domain.Session, domain.AuditEvent) error
	FindUserByUsername(context.Context, string) (domain.User, error)
	CreateLoginSession(context.Context, uuid.UUID, domain.Session, domain.AuditEvent, time.Time) error
	Authenticate(context.Context, []byte, time.Time, time.Duration, domain.AuditEvent) (domain.AuthenticatedSession, error)
	Logout(context.Context, uuid.UUID, time.Time, domain.AuditEvent) error
	RecordAudit(context.Context, domain.AuditEvent) error
	VerifyAuditChain(context.Context) error
	CreateHost(context.Context, domain.Host, domain.AuditEvent) error
	Hosts(context.Context) ([]domain.Host, error)
	Host(context.Context, uuid.UUID) (domain.Host, error)
	UpdateHost(context.Context, domain.Host, int64, domain.AuditEvent) (domain.Host, error)
	SetHostStatus(context.Context, uuid.UUID, int64, string, time.Time, domain.AuditEvent) (domain.Host, error)
	CreateEnrollmentToken(context.Context, domain.EnrollmentToken, domain.AuditEvent) error
	EnrollmentTokens(context.Context, uuid.UUID) ([]domain.EnrollmentToken, error)
	RevokeEnrollmentToken(context.Context, uuid.UUID, time.Time, domain.AuditEvent) error
	ConsumeEnrollmentToken(context.Context, []byte, time.Time, domain.EnrollmentIssuer) (domain.EnrollmentMaterial, error)
	AgentsForHost(context.Context, uuid.UUID) ([]domain.Agent, error)
	Agents(context.Context) ([]domain.Agent, error)
	Agent(context.Context, uuid.UUID) (domain.Agent, error)
	AgentByCertificate(context.Context, uuid.UUID, string, time.Time) (domain.Agent, error)
	MarkAgentConnected(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string, int64, time.Time) (domain.Agent, error)
	DesiredState(context.Context, uuid.UUID) (domain.DesiredState, error)
	MarkDesiredStatePublished(context.Context, uuid.UUID, int64, time.Time) error
	RecordAgentHeartbeat(context.Context, domain.AgentHeartbeat) (domain.Agent, error)
	RecordAgentInventory(context.Context, domain.AgentInventory) error
	LatestAgentInventory(context.Context, uuid.UUID) (domain.AgentInventory, error)
	AcceptAgentConfig(context.Context, uuid.UUID, domain.AgentConfigResult, time.Time) error
	RejectAgentConfig(context.Context, uuid.UUID, domain.AgentConfigResult, time.Time) error
	RevokeAgent(context.Context, uuid.UUID, string, time.Time, domain.AuditEvent) (domain.Agent, error)
	RotateAgentCertificate(context.Context, uuid.UUID, string, domain.AgentCertificate, time.Time, time.Time, domain.AuditEvent) error
	AgentCA(context.Context) (domain.AgentCARecord, error)
	InitializeAgentCA(context.Context, domain.AgentCARecord) (domain.AgentCARecord, error)
	StorageCredentials(context.Context, uuid.UUID, int) ([]domain.StorageCredential, error)
	StorageCredential(context.Context, uuid.UUID) (domain.StorageCredential, error)
	StorageCredentialSecret(context.Context, uuid.UUID) (domain.SecretEnvelope, error)
	SaveStorageCredential(context.Context, domain.StorageCredential, int64, *domain.SecretEnvelope, domain.AuditEvent) (domain.StorageCredential, error)
	Operation(context.Context, uuid.UUID) (domain.Operation, error)
	EnqueueCredentialTest(context.Context, domain.Operation, []byte, []byte, []byte, domain.AuditEvent) (domain.Operation, error)
	ClaimCredentialJob(context.Context, uuid.UUID) (domain.CredentialJob, error)
	RenewCredentialJob(context.Context, uuid.UUID, uuid.UUID) error
	RefreshCredentialJob(context.Context, uuid.UUID, uuid.UUID, int64, domain.SecretEnvelope) (domain.StorageCredential, error)
	CompleteCredentialJob(context.Context, uuid.UUID, uuid.UUID, string) error
}

// Settings controls security policy. Production defaults are applied to zero values.
type Settings struct {
	BootstrapToken    string
	IdleTTL           time.Duration
	AbsoluteTTL       time.Duration
	PasswordParams    security.Argon2Params
	ExpectedSchema    int
	Clock             func() time.Time
	Enrollment        EnrollmentSettings
	MasterKey         []byte
	RunCredentialTest CredentialTestRunner
}

// RequestMeta contains only non-secret request correlation data.
type RequestMeta struct {
	RequestID        uuid.UUID
	SourceIPHash     []byte
	UserAgentSummary string
}

type SessionCredentials struct {
	SessionToken string
	CSRFToken    string
}

type ValidationError struct {
	Field string
	Code  string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Code
}

// ControlPlane contains M1 auth and readiness rules without HTTP or SQL details.
type ControlPlane struct {
	store              Store
	bootstrapTokenHash []byte
	idleTTL            time.Duration
	absoluteTTL        time.Duration
	passwordParams     security.Argon2Params
	dummyPasswordHash  string
	expectedSchema     int
	clock              func() time.Time
	enrollment         EnrollmentSettings
	disconnectAgent    func(uuid.UUID)
	masterKey          []byte
	runCredentialTest  CredentialTestRunner
}

func NewControlPlane(store Store, settings Settings) (*ControlPlane, error) {
	if len(settings.MasterKey) != 0 && len(settings.MasterKey) != 32 {
		return nil, domain.ErrStorageUnavailable
	}
	if settings.IdleTTL == 0 {
		settings.IdleTTL = 30 * time.Minute
	}
	if settings.AbsoluteTTL == 0 {
		settings.AbsoluteTTL = 24 * time.Hour
	}
	if settings.PasswordParams.Memory == 0 {
		settings.PasswordParams = security.DefaultArgon2Params
	}
	if settings.ExpectedSchema == 0 {
		settings.ExpectedSchema = 6
	}
	if settings.Enrollment.HeartbeatInterval == 0 {
		settings.Enrollment.HeartbeatInterval = 15 * time.Second
	}
	if settings.Clock == nil {
		settings.Clock = time.Now
	}
	if settings.IdleTTL <= 0 || settings.AbsoluteTTL <= 0 || settings.IdleTTL > settings.AbsoluteTTL {
		return nil, errors.New("invalid session lifetime policy")
	}
	dummyHash, err := security.HashPassword("restfleet-dummy-password-value", settings.PasswordParams)
	if err != nil {
		return nil, err
	}
	return &ControlPlane{
		store:              store,
		bootstrapTokenHash: security.HashSecret(settings.BootstrapToken),
		idleTTL:            settings.IdleTTL,
		absoluteTTL:        settings.AbsoluteTTL,
		passwordParams:     settings.PasswordParams,
		dummyPasswordHash:  dummyHash,
		expectedSchema:     settings.ExpectedSchema,
		clock:              settings.Clock,
		enrollment:         settings.Enrollment,
		masterKey:          append([]byte(nil), settings.MasterKey...),
		runCredentialTest:  settings.RunCredentialTest,
	}, nil
}

func (c *ControlPlane) BootstrapRequired(ctx context.Context) (bool, error) {
	return c.store.BootstrapRequired(ctx)
}

func (c *ControlPlane) Bootstrap(
	ctx context.Context,
	token string,
	username string,
	displayName string,
	password string,
	meta RequestMeta,
) (domain.AuthenticatedSession, SessionCredentials, error) {
	if token == "" || !security.SecretHashMatches(c.bootstrapTokenHash, token) {
		if err := c.RecordDenied(ctx, "AUTH_BOOTSTRAP", "BOOTSTRAP", "INVALID_BOOTSTRAP_TOKEN", meta); err != nil {
			return domain.AuthenticatedSession{}, SessionCredentials{}, err
		}
		return domain.AuthenticatedSession{}, SessionCredentials{}, ErrInvalidBootstrapToken
	}
	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if err := validateAdmin(username, displayName, password); err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	passwordHash, err := security.HashPassword(password, c.passwordParams)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	now := c.clock().UTC()
	userID, err := uuid.NewV7()
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	user := domain.User{
		ID:           userID,
		Username:     strings.ToLower(username),
		DisplayName:  displayName,
		PasswordHash: passwordHash,
		Role:         domain.RoleAdmin,
		Status:       domain.UserActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	session, credentials, err := c.newSession(user.ID, now, meta)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	audit, err := c.auditEvent("AUTH_BOOTSTRAP", "USER", meta)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	if err := c.store.Bootstrap(ctx, user, session, audit); err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	return domain.AuthenticatedSession{User: user, Session: session}, credentials, nil
}

func (c *ControlPlane) Login(
	ctx context.Context,
	username string,
	password string,
	meta RequestMeta,
) (domain.AuthenticatedSession, SessionCredentials, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 || !validPasswordLength(password, 1) {
		return c.denyLogin(ctx, meta)
	}

	user, err := c.store.FindUserByUsername(ctx, username)
	if errors.Is(err, domain.ErrNotFound) {
		_, _ = security.VerifyPassword(c.dummyPasswordHash, password)
		return c.denyLogin(ctx, meta)
	}
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	valid, err := security.VerifyPassword(user.PasswordHash, password)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	if !valid || user.Status != domain.UserActive {
		return c.denyLogin(ctx, meta)
	}

	now := c.clock().UTC()
	session, credentials, err := c.newSession(user.ID, now, meta)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	audit, err := c.auditEvent("AUTH_LOGIN", "SESSION", meta)
	if err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	audit.ActorType = domain.ActorUser
	audit.ActorID = user.ID
	audit.ResourceID = session.ID
	audit.Result = domain.AuditSuccess
	audit.ReasonCode = "AUTHENTICATED"
	if err := c.store.CreateLoginSession(ctx, user.ID, session, audit, now); err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	return domain.AuthenticatedSession{User: user, Session: session}, credentials, nil
}

func (c *ControlPlane) Authenticate(
	ctx context.Context,
	sessionToken string,
	meta RequestMeta,
) (domain.AuthenticatedSession, error) {
	if sessionToken == "" {
		if err := c.RecordDenied(ctx, "AUTHORIZATION", "SESSION", "SESSION_MISSING", meta); err != nil {
			return domain.AuthenticatedSession{}, err
		}
		return domain.AuthenticatedSession{}, ErrUnauthorized
	}
	audit, err := c.auditEvent("AUTH_SESSION_EXPIRED", "SESSION", meta)
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	authenticated, err := c.store.Authenticate(
		ctx,
		security.HashSecret(sessionToken),
		c.clock().UTC(),
		c.idleTTL,
		audit,
	)
	if errors.Is(err, domain.ErrNotFound) {
		if auditErr := c.RecordDenied(ctx, "AUTHORIZATION", "SESSION", "SESSION_INVALID", meta); auditErr != nil {
			return domain.AuthenticatedSession{}, auditErr
		}
		return domain.AuthenticatedSession{}, ErrUnauthorized
	}
	if errors.Is(err, domain.ErrSessionExpired) {
		return domain.AuthenticatedSession{}, ErrUnauthorized
	}
	if err != nil {
		return domain.AuthenticatedSession{}, err
	}
	return authenticated, nil
}

func (c *ControlPlane) Logout(
	ctx context.Context,
	authenticated domain.AuthenticatedSession,
	csrfToken string,
	meta RequestMeta,
) error {
	if csrfToken == "" || !security.SecretHashMatches(authenticated.Session.CSRFSecretHash, csrfToken) {
		event, err := c.auditEvent("AUTH_LOGOUT", "SESSION", meta)
		if err != nil {
			return err
		}
		event.ActorType = domain.ActorUser
		event.ActorID = authenticated.User.ID
		event.ResourceID = authenticated.Session.ID
		event.Result = domain.AuditDenied
		event.ReasonCode = "CSRF_INVALID"
		if err := c.store.RecordAudit(ctx, event); err != nil {
			return err
		}
		return ErrCSRF
	}
	event, err := c.auditEvent("AUTH_LOGOUT", "SESSION", meta)
	if err != nil {
		return err
	}
	event.ActorType = domain.ActorUser
	event.ActorID = authenticated.User.ID
	event.ResourceID = authenticated.Session.ID
	event.Result = domain.AuditSuccess
	event.ReasonCode = "SESSION_REVOKED"
	return c.store.Logout(ctx, authenticated.Session.ID, c.clock().UTC(), event)
}

func (c *ControlPlane) Ready(ctx context.Context) (map[string]string, error) {
	checks := map[string]string{"database": "unavailable", "schema": "unknown", "audit": "unknown"}
	if err := c.store.Ping(ctx); err != nil {
		return checks, err
	}
	checks["database"] = "ok"
	version, err := c.store.SchemaVersion(ctx)
	if err != nil {
		return checks, err
	}
	if version != c.expectedSchema {
		checks["schema"] = "incompatible"
		return checks, errors.New("database schema is incompatible")
	}
	checks["schema"] = "ok"
	if err := c.store.VerifyAuditChain(ctx); err != nil {
		checks["audit"] = "invalid"
		return checks, errors.New("audit chain verification failed")
	}
	checks["audit"] = "ok"
	return checks, nil
}

func (c *ControlPlane) VerifyAuditChain(ctx context.Context) error {
	return c.store.VerifyAuditChain(ctx)
}

func (c *ControlPlane) denyLogin(
	ctx context.Context,
	meta RequestMeta,
) (domain.AuthenticatedSession, SessionCredentials, error) {
	if err := c.RecordDenied(ctx, "AUTH_LOGIN", "SESSION", "INVALID_CREDENTIALS", meta); err != nil {
		return domain.AuthenticatedSession{}, SessionCredentials{}, err
	}
	return domain.AuthenticatedSession{}, SessionCredentials{}, ErrInvalidCredentials
}

func (c *ControlPlane) RecordDenied(
	ctx context.Context,
	action string,
	resourceType string,
	reason string,
	meta RequestMeta,
) error {
	event, err := c.auditEvent(action, resourceType, meta)
	if err != nil {
		return err
	}
	event.Result = domain.AuditDenied
	event.ReasonCode = reason
	return c.store.RecordAudit(ctx, event)
}

func (c *ControlPlane) auditEvent(
	action string,
	resourceType string,
	meta RequestMeta,
) (domain.AuditEvent, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return domain.AuditEvent{}, err
	}
	if meta.RequestID == uuid.Nil {
		meta.RequestID, err = uuid.NewV7()
		if err != nil {
			return domain.AuditEvent{}, err
		}
	}
	return domain.AuditEvent{
		ID:           id,
		OccurredAt:   c.clock().UTC(),
		ActorType:    domain.ActorSystem,
		Action:       action,
		ResourceType: resourceType,
		RequestID:    meta.RequestID,
		SourceIPHash: append([]byte(nil), meta.SourceIPHash...),
		Result:       domain.AuditFailure,
		ReasonCode:   "UNSPECIFIED",
		Changes:      json.RawMessage("{}"),
	}, nil
}

func (c *ControlPlane) newSession(
	userID uuid.UUID,
	now time.Time,
	meta RequestMeta,
) (domain.Session, SessionCredentials, error) {
	sessionToken, err := security.NewOpaqueToken()
	if err != nil {
		return domain.Session{}, SessionCredentials{}, err
	}
	csrfToken, err := security.NewOpaqueToken()
	if err != nil {
		return domain.Session{}, SessionCredentials{}, err
	}
	sessionID, err := uuid.NewV7()
	if err != nil {
		return domain.Session{}, SessionCredentials{}, err
	}
	idleExpiry := now.Add(c.idleTTL)
	absoluteExpiry := now.Add(c.absoluteTTL)
	if idleExpiry.After(absoluteExpiry) {
		idleExpiry = absoluteExpiry
	}
	return domain.Session{
		ID:                sessionID,
		UserID:            userID,
		TokenHash:         security.HashSecret(sessionToken),
		CSRFSecretHash:    security.HashSecret(csrfToken),
		CreatedAt:         now,
		LastSeenAt:        now,
		IdleExpiresAt:     idleExpiry,
		AbsoluteExpiresAt: absoluteExpiry,
		IPHash:            append([]byte(nil), meta.SourceIPHash...),
		UserAgentSummary:  truncateRunes(meta.UserAgentSummary, 256),
	}, SessionCredentials{SessionToken: sessionToken, CSRFToken: csrfToken}, nil
}

func validateAdmin(username, displayName, password string) error {
	if len(username) < 3 || len(username) > 64 || !usernamePattern.MatchString(username) {
		return &ValidationError{Field: "username", Code: "INVALID_USERNAME"}
	}
	if displayName == "" || utf8.RuneCountInString(displayName) > 128 {
		return &ValidationError{Field: "display_name", Code: "INVALID_DISPLAY_NAME"}
	}
	if !validPasswordLength(password, 12) {
		return &ValidationError{Field: "password", Code: "INVALID_PASSWORD"}
	}
	return nil
}

func validPasswordLength(password string, minimum int) bool {
	length := utf8.RuneCountInString(password)
	return utf8.ValidString(password) && length >= minimum && length <= maxPasswordRunes && len(password) <= maxPasswordBytes
}

func truncateRunes(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}
