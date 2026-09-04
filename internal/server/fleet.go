package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

var (
	ErrInvalidEnrollment = errors.New("invalid enrollment request")
	ErrIncompatibleAgent = errors.New("incompatible agent")
	ErrInvalidRevision   = errors.New("invalid revision")
)

var labelKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

type EnrollmentSettings struct {
	Pepper            []byte
	CA                *security.AgentCA
	PublicURL         string
	GRPCEndpoint      string
	ServerName        string
	ServerCABundlePEM []byte
	HeartbeatInterval time.Duration
}

type EnrollmentRequest struct {
	Token           string
	CSRPEM          string
	InstallID       string
	AgentVersion    string
	ProtocolVersion string
	Hostname        string
	OS              string
	Arch            string
	Capabilities    []string
}

type EnrollmentResponse struct {
	AgentID          uuid.UUID
	HostID           uuid.UUID
	CertificatePEM   string
	CABundlePEM      string
	NotAfter         time.Time
	ServerName       string
	GRPCEndpoint     string
	HeartbeatSeconds int
}

type EnrollmentTokenCreated struct {
	Token  domain.EnrollmentToken
	Secret string
	Native string
	Docker string
}

type HostPatch struct {
	DisplayName *string
	Description *string
	Labels      *map[string]string
	Timezone    *string
}

func (c *ControlPlane) Hosts(ctx context.Context) ([]domain.Host, error) {
	return c.store.Hosts(ctx)
}

func (c *ControlPlane) Host(ctx context.Context, id uuid.UUID) (domain.Host, error) {
	return c.store.Host(ctx, id)
}

func (c *ControlPlane) CreateHost(
	ctx context.Context,
	displayName, description, timezone string,
	labels map[string]string,
	actor domain.User,
	meta RequestMeta,
) (domain.Host, error) {
	host := domain.Host{
		DisplayName: strings.TrimSpace(displayName),
		Description: strings.TrimSpace(description),
		Labels:      cloneLabels(labels),
		Timezone:    strings.TrimSpace(timezone),
		Status:      domain.HostPending,
		Revision:    1,
	}
	if err := validateHost(host); err != nil {
		return domain.Host{}, err
	}
	var err error
	host.ID, err = uuid.NewV7()
	if err != nil {
		return domain.Host{}, err
	}
	host.CreatedAt = c.clock().UTC()
	host.UpdatedAt = host.CreatedAt
	audit, err := c.userAudit("HOST_CREATE", "HOST", host.ID, actor.ID, meta, "HOST_CREATED")
	if err != nil {
		return domain.Host{}, err
	}
	if err := c.store.CreateHost(ctx, host, audit); err != nil {
		return domain.Host{}, err
	}
	return host, nil
}

func (c *ControlPlane) UpdateHost(
	ctx context.Context,
	id uuid.UUID,
	expectedRevision int64,
	patch HostPatch,
	actor domain.User,
	meta RequestMeta,
) (domain.Host, error) {
	if expectedRevision < 1 {
		return domain.Host{}, ErrInvalidRevision
	}
	if patch.DisplayName == nil && patch.Description == nil && patch.Labels == nil && patch.Timezone == nil {
		return domain.Host{}, &ValidationError{Field: "body", Code: "EMPTY_PATCH"}
	}
	host, err := c.store.Host(ctx, id)
	if err != nil {
		return domain.Host{}, err
	}
	if patch.DisplayName != nil {
		host.DisplayName = strings.TrimSpace(*patch.DisplayName)
	}
	if patch.Description != nil {
		host.Description = strings.TrimSpace(*patch.Description)
	}
	if patch.Labels != nil {
		host.Labels = cloneLabels(*patch.Labels)
	}
	if patch.Timezone != nil {
		host.Timezone = strings.TrimSpace(*patch.Timezone)
	}
	if err := validateHost(host); err != nil {
		return domain.Host{}, err
	}
	host.UpdatedAt = c.clock().UTC()
	audit, err := c.userAudit("HOST_UPDATE", "HOST", id, actor.ID, meta, "HOST_UPDATED")
	if err != nil {
		return domain.Host{}, err
	}
	return c.store.UpdateHost(ctx, host, expectedRevision, audit)
}

func (c *ControlPlane) SetHostEnabled(
	ctx context.Context,
	id uuid.UUID,
	expectedRevision int64,
	enabled bool,
	actor domain.User,
	meta RequestMeta,
) (domain.Host, error) {
	if expectedRevision < 1 {
		return domain.Host{}, ErrInvalidRevision
	}
	action, command, reason := "HOST_DISABLE", "DISABLE", "HOST_DISABLED"
	if enabled {
		action, command, reason = "HOST_ENABLE", "ENABLE", "HOST_ENABLED"
	}
	audit, err := c.userAudit(action, "HOST", id, actor.ID, meta, reason)
	if err != nil {
		return domain.Host{}, err
	}
	return c.store.SetHostStatus(ctx, id, expectedRevision, command, c.clock().UTC(), audit)
}

func validateHost(host domain.Host) error {
	if host.DisplayName == "" || utf8.RuneCountInString(host.DisplayName) > 128 {
		return &ValidationError{Field: "display_name", Code: "INVALID_DISPLAY_NAME"}
	}
	if utf8.RuneCountInString(host.Description) > 1024 {
		return &ValidationError{Field: "description", Code: "INVALID_DESCRIPTION"}
	}
	if host.Timezone == "" {
		return &ValidationError{Field: "timezone", Code: "INVALID_TIMEZONE"}
	}
	if _, err := time.LoadLocation(host.Timezone); err != nil {
		return &ValidationError{Field: "timezone", Code: "INVALID_TIMEZONE"}
	}
	if len(host.Labels) > 32 {
		return &ValidationError{Field: "labels", Code: "TOO_MANY_LABELS"}
	}
	for key, value := range host.Labels {
		if !labelKeyPattern.MatchString(key) || utf8.RuneCountInString(value) > 128 {
			return &ValidationError{Field: "labels", Code: "INVALID_LABEL"}
		}
	}
	return nil
}

func cloneLabels(labels map[string]string) map[string]string {
	result := make(map[string]string, len(labels))
	for key, value := range labels {
		result[key] = value
	}
	return result
}

func (c *ControlPlane) CreateEnrollmentToken(
	ctx context.Context,
	hostID uuid.UUID,
	expiresInSeconds int,
	actor domain.User,
	meta RequestMeta,
) (EnrollmentTokenCreated, error) {
	if c.enrollment.CA == nil || len(c.enrollment.Pepper) < 32 || c.enrollment.PublicURL == "" {
		return EnrollmentTokenCreated{}, domain.ErrEnrollmentUnavailable
	}
	if expiresInSeconds == 0 {
		expiresInSeconds = 600
	}
	if expiresInSeconds < 1 || expiresInSeconds > 3600 {
		return EnrollmentTokenCreated{}, &ValidationError{Field: "expires_in_seconds", Code: "INVALID_TTL"}
	}
	secret, err := security.NewEnrollmentToken()
	if err != nil {
		return EnrollmentTokenCreated{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return EnrollmentTokenCreated{}, err
	}
	now := c.clock().UTC()
	token := domain.EnrollmentToken{
		ID: id, HostID: hostID,
		TokenHash:   security.HashEnrollmentToken(c.enrollment.Pepper, secret),
		Fingerprint: security.EnrollmentTokenFingerprint(secret),
		ExpiresAt:   now.Add(time.Duration(expiresInSeconds) * time.Second),
		CreatedBy:   actor.ID, CreatedAt: now,
	}
	audit, err := c.userAudit("ENROLLMENT_TOKEN_CREATE", "ENROLLMENT_TOKEN", id, actor.ID, meta, "TOKEN_CREATED")
	if err != nil {
		return EnrollmentTokenCreated{}, err
	}
	if err := c.store.CreateEnrollmentToken(ctx, token, audit); err != nil {
		return EnrollmentTokenCreated{}, err
	}
	serverURL := strings.TrimRight(c.enrollment.PublicURL, "/")
	return EnrollmentTokenCreated{
		Token:  token,
		Secret: secret,
		Native: fmt.Sprintf("sudo -u restfleet-agent restfleet-agent enroll --server '%s' --token-file /dev/stdin", serverURL),
		Docker: fmt.Sprintf("docker run --rm -i --network host -v restfleet-agent-state:/var/lib/restfleet-agent ghcr.io/sagehou/restfleet-agent enroll --server '%s' --token-file /dev/stdin", serverURL),
	}, nil
}

func (c *ControlPlane) EnrollmentTokens(ctx context.Context, hostID uuid.UUID) ([]domain.EnrollmentToken, error) {
	if _, err := c.store.Host(ctx, hostID); err != nil {
		return nil, err
	}
	return c.store.EnrollmentTokens(ctx, hostID)
}

func (c *ControlPlane) RevokeEnrollmentToken(
	ctx context.Context,
	id uuid.UUID,
	actor domain.User,
	meta RequestMeta,
) error {
	audit, err := c.userAudit("ENROLLMENT_TOKEN_REVOKE", "ENROLLMENT_TOKEN", id, actor.ID, meta, "TOKEN_REVOKED")
	if err != nil {
		return err
	}
	return c.store.RevokeEnrollmentToken(ctx, id, c.clock().UTC(), audit)
}

func (c *ControlPlane) EnrollAgent(
	ctx context.Context,
	request EnrollmentRequest,
	meta RequestMeta,
) (EnrollmentResponse, error) {
	if c.enrollment.CA == nil || len(c.enrollment.Pepper) < 32 ||
		c.enrollment.GRPCEndpoint == "" || c.enrollment.ServerName == "" ||
		len(c.enrollment.ServerCABundlePEM) == 0 {
		return EnrollmentResponse{}, domain.ErrEnrollmentUnavailable
	}
	if !security.ValidEnrollmentToken(request.Token) {
		if err := c.RecordDenied(ctx, "AGENT_ENROLL", "AGENT", "TOKEN_INVALID", meta); err != nil {
			return EnrollmentResponse{}, err
		}
		return EnrollmentResponse{}, domain.ErrEnrollmentTokenInvalid
	}
	now := c.clock().UTC()
	material, err := c.store.ConsumeEnrollmentToken(
		ctx,
		security.HashEnrollmentToken(c.enrollment.Pepper, request.Token),
		now,
		func(hostID uuid.UUID) (domain.EnrollmentMaterial, error) {
			installID, err := uuid.Parse(request.InstallID)
			if err != nil || !validAgentMetadata(request) {
				return domain.EnrollmentMaterial{}, ErrInvalidEnrollment
			}
			if _, err := security.ParseAgentCSR([]byte(request.CSRPEM)); err != nil {
				return domain.EnrollmentMaterial{}, ErrInvalidEnrollment
			}
			if !supportedProtocol(request.ProtocolVersion) {
				return domain.EnrollmentMaterial{}, ErrIncompatibleAgent
			}
			agentID, err := uuid.NewV7()
			if err != nil {
				return domain.EnrollmentMaterial{}, err
			}
			issued, err := c.enrollment.CA.IssueAgentCertificate([]byte(request.CSRPEM), agentID, now)
			if err != nil {
				return domain.EnrollmentMaterial{}, ErrInvalidEnrollment
			}
			certificateID, err := uuid.NewV7()
			if err != nil {
				return domain.EnrollmentMaterial{}, err
			}
			desiredState, err := domain.NewDefaultDesiredState(agentID, 1, now)
			if err != nil {
				return domain.EnrollmentMaterial{}, err
			}
			outboxID, err := uuid.NewV7()
			if err != nil {
				return domain.EnrollmentMaterial{}, err
			}
			agent := domain.Agent{
				ID: agentID, HostID: hostID, InstallID: installID,
				PublicKeyFingerprint: issued.PublicKeyFingerprint,
				CertificateSerial:    issued.SerialNumber,
				CertificateNotAfter:  issued.NotAfter,
				Status:               domain.AgentActive, Version: request.AgentVersion,
				ProtocolVersion: request.ProtocolVersion, OS: request.OS,
				Arch: request.Arch, Hostname: request.Hostname,
				DesiredRevision: 1,
				CreatedAt:       now, UpdatedAt: now,
			}
			certificate := domain.AgentCertificate{
				ID: certificateID, AgentID: agentID, SerialNumber: issued.SerialNumber,
				PublicKeyFingerprint: issued.PublicKeyFingerprint,
				NotBefore:            issued.NotBefore, NotAfter: issued.NotAfter, IssuedAt: now,
			}
			audit, err := c.auditEvent("AGENT_ENROLL", "AGENT", meta)
			if err != nil {
				return domain.EnrollmentMaterial{}, err
			}
			audit.ActorType = domain.ActorAgent
			audit.ActorID = agentID
			audit.ResourceID = agentID
			audit.Result = domain.AuditSuccess
			audit.ReasonCode = "AGENT_ENROLLED"
			return domain.EnrollmentMaterial{
				Agent: agent, Certificate: certificate, DesiredState: desiredState,
				CertificatePEM: issued.CertificatePEM, Audit: audit,
				OutboxID: outboxID,
			}, nil
		},
	)
	if err != nil {
		reason := ""
		switch {
		case errors.Is(err, domain.ErrEnrollmentTokenInvalid):
			reason = "TOKEN_INVALID"
		case errors.Is(err, ErrInvalidEnrollment):
			reason = "REQUEST_INVALID"
		case errors.Is(err, ErrIncompatibleAgent):
			reason = "PROTOCOL_INCOMPATIBLE"
		}
		if reason != "" {
			if auditErr := c.RecordDenied(ctx, "AGENT_ENROLL", "AGENT", reason, meta); auditErr != nil {
				return EnrollmentResponse{}, auditErr
			}
		}
		return EnrollmentResponse{}, err
	}
	return EnrollmentResponse{
		AgentID: material.Agent.ID, HostID: material.Agent.HostID,
		CertificatePEM:   string(material.CertificatePEM),
		CABundlePEM:      string(c.enrollment.ServerCABundlePEM),
		NotAfter:         material.Certificate.NotAfter,
		ServerName:       c.enrollment.ServerName,
		GRPCEndpoint:     c.enrollment.GRPCEndpoint,
		HeartbeatSeconds: int(c.enrollment.HeartbeatInterval.Seconds()),
	}, nil
}

func validAgentMetadata(request EnrollmentRequest) bool {
	return request.OS == "linux" &&
		(request.Arch == "amd64" || request.Arch == "arm64") &&
		strings.TrimSpace(request.Hostname) != "" &&
		utf8.RuneCountInString(request.Hostname) <= 255 &&
		strings.TrimSpace(request.AgentVersion) != "" &&
		len(request.AgentVersion) <= 64 &&
		len(request.ProtocolVersion) <= 16 &&
		len(request.Capabilities) <= 64
}

func supportedProtocol(version string) bool {
	return version == "1.0" || version == "0.9"
}

func (c *ControlPlane) AgentsForHost(ctx context.Context, hostID uuid.UUID) ([]domain.Agent, error) {
	if _, err := c.store.Host(ctx, hostID); err != nil {
		return nil, err
	}
	agents, err := c.store.AgentsForHost(ctx, hostID)
	if err != nil {
		return nil, err
	}
	return c.withAgentHealth(agents), nil
}

func (c *ControlPlane) Agents(ctx context.Context) ([]domain.Agent, error) {
	agents, err := c.store.Agents(ctx)
	if err != nil {
		return nil, err
	}
	return c.withAgentHealth(agents), nil
}

func (c *ControlPlane) Agent(ctx context.Context, id uuid.UUID) (domain.Agent, error) {
	agent, err := c.store.Agent(ctx, id)
	if err != nil {
		return domain.Agent{}, err
	}
	agent.Health = agent.HealthAt(c.clock().UTC(), agentOfflineAfter)
	return agent, nil
}

func (c *ControlPlane) AgentByCertificate(
	ctx context.Context,
	id uuid.UUID,
	serial string,
	now time.Time,
) (domain.Agent, error) {
	return c.store.AgentByCertificate(ctx, id, serial, now)
}

func (c *ControlPlane) MarkAgentConnected(
	ctx context.Context,
	id, installID uuid.UUID,
	version, protocolVersion, hostname, bootID, resticVersion string,
	acceptedRevision int64,
) (domain.Agent, error) {
	if acceptedRevision < 0 {
		return domain.Agent{}, &ValidationError{Field: "accepted_config_revision", Code: "INVALID_REVISION"}
	}
	return c.store.MarkAgentConnected(
		ctx, id, installID, version, protocolVersion, hostname, bootID,
		resticVersion, acceptedRevision, c.clock().UTC(),
	)
}

func (c *ControlPlane) RevokeAgent(
	ctx context.Context,
	id uuid.UUID,
	reason string,
	actor domain.User,
	meta RequestMeta,
) (domain.Agent, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > 512 {
		return domain.Agent{}, &ValidationError{Field: "reason", Code: "INVALID_REASON"}
	}
	audit, err := c.userAudit("AGENT_REVOKE", "AGENT", id, actor.ID, meta, "AGENT_REVOKED")
	if err != nil {
		return domain.Agent{}, err
	}
	agent, err := c.store.RevokeAgent(ctx, id, reason, c.clock().UTC(), audit)
	if err == nil && c.disconnectAgent != nil {
		c.disconnectAgent(id)
	}
	return agent, err
}

func (c *ControlPlane) RotateAgentCertificate(
	ctx context.Context,
	agentID uuid.UUID,
	currentSerial string,
	csrPEM []byte,
	meta RequestMeta,
) (security.IssuedAgentCertificate, error) {
	if c.enrollment.CA == nil {
		return security.IssuedAgentCertificate{}, domain.ErrEnrollmentUnavailable
	}
	if _, err := security.ParseAgentCSR(csrPEM); err != nil {
		if auditErr := c.RecordDenied(ctx, "AGENT_CERTIFICATE_ROTATE", "AGENT_CERTIFICATE", "CSR_INVALID", meta); auditErr != nil {
			return security.IssuedAgentCertificate{}, auditErr
		}
		return security.IssuedAgentCertificate{}, ErrInvalidEnrollment
	}
	now := c.clock().UTC()
	if _, err := c.store.AgentByCertificate(ctx, agentID, currentSerial, now); err != nil {
		if auditErr := c.RecordDenied(ctx, "AGENT_CERTIFICATE_ROTATE", "AGENT_CERTIFICATE", "CERTIFICATE_INVALID", meta); auditErr != nil {
			return security.IssuedAgentCertificate{}, auditErr
		}
		return security.IssuedAgentCertificate{}, err
	}
	issued, err := c.enrollment.CA.IssueAgentCertificate(csrPEM, agentID, now)
	if err != nil {
		return security.IssuedAgentCertificate{}, err
	}
	certificateID, err := uuid.NewV7()
	if err != nil {
		return security.IssuedAgentCertificate{}, err
	}
	certificate := domain.AgentCertificate{
		ID: certificateID, AgentID: agentID, SerialNumber: issued.SerialNumber,
		PublicKeyFingerprint: issued.PublicKeyFingerprint,
		NotBefore:            issued.NotBefore, NotAfter: issued.NotAfter, IssuedAt: now,
	}
	audit, err := c.auditEvent("AGENT_CERTIFICATE_ROTATE", "AGENT_CERTIFICATE", meta)
	if err != nil {
		return security.IssuedAgentCertificate{}, err
	}
	audit.ActorType = domain.ActorAgent
	audit.ActorID = agentID
	audit.ResourceID = certificateID
	audit.Result = domain.AuditSuccess
	audit.ReasonCode = "CERTIFICATE_ROTATED"
	if err := c.store.RotateAgentCertificate(
		ctx, agentID, currentSerial, certificate, now, now.Add(24*time.Hour), audit,
	); err != nil {
		return security.IssuedAgentCertificate{}, err
	}
	return issued, nil
}

func (c *ControlPlane) SetAgentDisconnector(disconnect func(uuid.UUID)) {
	c.disconnectAgent = disconnect
}

func (c *ControlPlane) userAudit(
	action, resourceType string,
	resourceID, actorID uuid.UUID,
	meta RequestMeta,
	reason string,
) (domain.AuditEvent, error) {
	event, err := c.auditEvent(action, resourceType, meta)
	if err != nil {
		return domain.AuditEvent{}, err
	}
	event.ActorType = domain.ActorUser
	event.ActorID = actorID
	event.ResourceID = resourceID
	event.Result = domain.AuditSuccess
	event.ReasonCode = reason
	event.Changes = json.RawMessage("{}")
	return event, nil
}

func ValidateEnrollmentPublicURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return errors.New("enrollment public URL must be an https URL without user info")
	}
	return nil
}
