package gateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
)

var (
	ErrProvision  = errors.New("gateway runtime provisioning unavailable or inconsistent")
	ErrFinalize   = errors.New("gateway runtime finalization unavailable or inconsistent")
	maxProvision  = 32 << 10
	maxPlan       = 16 << 10
	maxRegistr    = 4096
)

// ProvisionConfig is protected Gateway-local static metadata for one fresh
// runtime. It contains paths, public pins and ranges only; source seeds are
// created locally and never enter this file.
type ProvisionConfig struct {
	Version                  int    `json:"version"`
	Environment              string `json:"environment"`
	AgentID                  uuid.UUID `json:"agent_id"`
	AdmissionLifetimeSeconds int64  `json:"admission_lifetime_seconds"`
	LifetimeSeconds          int64  `json:"lifetime_seconds"`
	MaterialSocket           string `json:"material_socket"`
	AuthoritySocket          string `json:"authority_socket"`
	GatewayUID               uint32 `json:"gateway_uid"`
	SharedGroup              uint32 `json:"shared_group"`
	CentralPinFile           string `json:"central_pin_file"`
	AuditSourceFile          string `json:"audit_source_file"`
	RepositorySourceFile     string `json:"repository_source_file"`
	AuditQueueDirectory      string `json:"audit_queue_directory"`
	QueueDirectory           string `json:"queue_directory"`
	RuntimeDirectory         string `json:"runtime_directory"`
	RcloneBinary             string `json:"rclone_binary"`
	CertificateFile          string `json:"certificate_file"`
	TLSKeyFile               string `json:"tls_key_file"`
	ClientCAFile             string `json:"client_ca_file"`
	ListenAddress            string `json:"listen_address"`
	MaxBytes                 int64  `json:"max_bytes"`
	MaxRecords               int    `json:"max_records"`
	MaxSessions              int    `json:"max_sessions"`
	ServerUID                uint32 `json:"server_uid"`
	ReplaySocket             string `json:"replay_socket"`
	StartupWaitSeconds       int    `json:"startup_wait_seconds"`
}

// ProvisionState is durable public coordination metadata. It contains the
// generated runtime/origin IDs, source PUBLIC keys and the exact config that
// produced them. It is not proof of registration, freshness or readiness.
type ProvisionState struct {
	Version                int                        `json:"version"`
	Config                 ProvisionConfig            `json:"config"`
	RuntimeID              uuid.UUID                  `json:"runtime_id"`
	AuditOrigin            domain.GatewayAuditBinding `json:"audit_origin"`
	AuditSourcePublic      ed25519.PublicKey          `json:"audit_source_public"`
	RepositorySourcePublic ed25519.PublicKey          `json:"repository_source_public"`
}

type ProvisionPreparation struct {
	Version                  int               `json:"version"`
	AgentID                  uuid.UUID         `json:"agent_id"`
	RuntimeID                uuid.UUID         `json:"runtime_id"`
	SourcePublic             ed25519.PublicKey `json:"source_public"`
	AdmissionLifetimeSeconds int64             `json:"admission_lifetime_seconds"`
	LifetimeSeconds          int64             `json:"lifetime_seconds"`
	MaterialSocket           string            `json:"material_socket"`
	AuthoritySocket          string            `json:"authority_socket"`
	GatewayUID               uint32            `json:"gateway_uid"`
	SharedGroup              uint32            `json:"shared_group"`
}

type ProvisionAuditRegistration struct {
	Version      int                        `json:"version"`
	AuditOrigin  domain.GatewayAuditBinding `json:"audit_origin"`
	SourcePublic ed25519.PublicKey          `json:"source_public"`
}

type ProvisionOutput struct {
	Version           int                       `json:"version"`
	Preparation       ProvisionPreparation      `json:"preparation"`
	AuditRegistration ProvisionAuditRegistration `json:"audit_registration"`
}

func (c ProvisionConfig) validate() bool {
	if c.Version != 1 || (c.Environment != "production" && c.Environment != "development" && c.Environment != "test") ||
		c.AgentID.Version() != 7 || c.AgentID.Variant() != uuid.RFC4122 ||
		c.AdmissionLifetimeSeconds < 60 || c.AdmissionLifetimeSeconds > int64(domain.MaxGatewayOfflineWindow/time.Second) ||
		c.LifetimeSeconds < 0 || c.LifetimeSeconds > security.MaxGatewayAuthorizationSeconds ||
		c.MaterialSocket == c.AuthoritySocket || c.MaxBytes < security.MaxGatewayPendingSize+(16<<10) || c.MaxBytes > 64<<20 ||
		c.MaxRecords < 1 || c.MaxRecords > 4096 || c.MaxSessions < 1 || c.MaxSessions > 32 ||
		c.StartupWaitSeconds < 1 || c.StartupWaitSeconds > 300 || c.ServerUID == ^uint32(0) || c.SharedGroup == ^uint32(0) {
		return false
	}
	uid := uint32(os.Geteuid())
	if c.GatewayUID != uid ||
		(c.SharedGroup == 0 && c.ServerUID != uid) ||
		(c.SharedGroup != 0 && (uid == 0 || c.ServerUID == 0 || c.ServerUID == uid)) ||
		(c.Environment == "production" && (uid == 0 || c.SharedGroup == 0)) {
		return false
	}
	paths := []string{c.CentralPinFile, c.AuditSourceFile, c.RepositorySourceFile, c.AuditQueueDirectory, c.QueueDirectory,
		c.RuntimeDirectory, c.RcloneBinary, c.CertificateFile, c.TLSKeyFile, c.MaterialSocket, c.AuthoritySocket, c.ReplaySocket, c.ClientCAFile}
	seen := make(map[string]bool)
	for _, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.IndexFunc(p, unicode.IsControl) >= 0 || seen[p] {
			return false
		}
		seen[p] = true
	}
	if len(c.MaterialSocket) > 107 || len(c.AuthoritySocket) > 107 || len(c.ReplaySocket) > 107 ||
		c.AuditQueueDirectory == c.QueueDirectory || c.AuditSourceFile == c.RepositorySourceFile ||
		c.AuditSourceFile == c.CentralPinFile || c.RepositorySourceFile == c.CentralPinFile {
		return false
	}
	host, port, err := net.SplitHostPort(c.ListenAddress)
	n, numberErr := strconv.ParseUint(port, 10, 16)
	if err != nil || numberErr != nil || net.ParseIP(host) == nil || strconv.FormatUint(n, 10) != port ||
		net.JoinHostPort(host, port) != c.ListenAddress || (c.Environment == "production" && n == 0) {
		return false
	}
	return true
}

func LoadProvisionConfig(path string) (ProvisionConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, int64(maxProvision))
	if err != nil {
		return ProvisionConfig{}, ErrProvision
	}
	defer clear(raw)
	var c ProvisionConfig
	if json.Unmarshal(raw, &c) != nil || !c.validate() {
		return ProvisionConfig{}, ErrProvision
	}
	canonical, err := json.Marshal(c)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return ProvisionConfig{}, ErrProvision
	}
	return c, nil
}

func LoadProvisionState(path string) (ProvisionState, error) {
	raw, err := security.ReadProtectedGatewayFile(path, int64(maxProvision))
	if err != nil {
		return ProvisionState{}, ErrProvision
	}
	defer clear(raw)
	var s ProvisionState
	if json.Unmarshal(raw, &s) != nil || s.Version != 1 || !s.Config.validate() ||
		s.RuntimeID.Version() != 7 || s.RuntimeID.Variant() != uuid.RFC4122 ||
		s.AuditOrigin.Validate() != nil || s.AuditOrigin.RuntimeID != s.RuntimeID ||
		len(s.AuditSourcePublic) != ed25519.PublicKeySize || len(s.RepositorySourcePublic) != ed25519.PublicKeySize ||
		bytes.Equal(s.AuditSourcePublic, s.RepositorySourcePublic) {
		return ProvisionState{}, ErrProvision
	}
	canonical, err := json.Marshal(s)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return ProvisionState{}, ErrProvision
	}
	return s, nil
}

// Provision creates the fresh local sources/Queue and a durable state file, or
// verifies an exact retry. It never contacts the control plane and never
// overwrites a source seed. The caller copies the public output to central
// protected metadata; private seeds stay on the Gateway host.
func Provision(config ProvisionConfig, statePath string) (ProvisionOutput, error) {
	if !config.validate() || statePath == "" || !filepath.IsAbs(statePath) || filepath.Clean(statePath) != statePath {
		return ProvisionOutput{}, ErrProvision
	}
	if _, err := os.Lstat(statePath); err == nil {
		return retryProvision(config, statePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ProvisionOutput{}, ErrProvision
	}
	for _, dir := range []string{config.AuditQueueDirectory, config.QueueDirectory, config.RuntimeDirectory} {
		if err := ensurePrivateDirectory(dir); err != nil {
			return ProvisionOutput{}, ErrProvision
		}
	}
	if err := validateProvisionStatic(config); err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	runtimeID, err := uuid.NewV7()
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	originID, err := uuid.NewV7()
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	auditPublic, err := security.CreateGatewaySource(config.AuditSourceFile)
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	repositoryPublic, err := security.CreateGatewaySource(config.RepositorySourceFile)
	if err != nil || bytes.Equal(auditPublic, repositoryPublic) {
		return ProvisionOutput{}, ErrProvision
	}
	state := ProvisionState{Version: 1, Config: config, RuntimeID: runtimeID,
		AuditOrigin: domain.GatewayAuditBinding{OriginID: originID, RuntimeID: runtimeID},
		AuditSourcePublic: auditPublic, RepositorySourcePublic: repositoryPublic}
	raw, err := json.Marshal(state)
	if err != nil || security.PersistProtectedGatewayFile(statePath, raw) != nil {
		return ProvisionOutput{}, ErrProvision
	}
	return provisionOutput(state), nil
}

func retryProvision(config ProvisionConfig, statePath string) (ProvisionOutput, error) {
	state, err := LoadProvisionState(statePath)
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	expected, err := json.Marshal(config)
	actual, marshalErr := json.Marshal(state.Config)
	if err != nil || marshalErr != nil || !bytes.Equal(expected, actual) {
		return ProvisionOutput{}, ErrProvision
	}
	auditKey, err := security.LoadGatewaySource(config.AuditSourceFile)
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	defer clear(auditKey)
	repositoryKey, err := security.LoadGatewaySource(config.RepositorySourceFile)
	if err != nil {
		return ProvisionOutput{}, ErrProvision
	}
	defer clear(repositoryKey)
	if !bytes.Equal(auditKey.Public().(ed25519.PublicKey), state.AuditSourcePublic) ||
		!bytes.Equal(repositoryKey.Public().(ed25519.PublicKey), state.RepositorySourcePublic) {
		return ProvisionOutput{}, ErrProvision
	}
	return provisionOutput(state), nil
}

func provisionOutput(state ProvisionState) ProvisionOutput {
	c := state.Config
	return ProvisionOutput{Version: 1,
		Preparation: ProvisionPreparation{Version: 1, AgentID: c.AgentID, RuntimeID: state.RuntimeID,
			SourcePublic: append(ed25519.PublicKey(nil), state.RepositorySourcePublic...),
			AdmissionLifetimeSeconds: c.AdmissionLifetimeSeconds, LifetimeSeconds: c.LifetimeSeconds,
			MaterialSocket: c.MaterialSocket, AuthoritySocket: c.AuthoritySocket,
			GatewayUID: c.GatewayUID, SharedGroup: c.SharedGroup},
		AuditRegistration: ProvisionAuditRegistration{Version: 1, AuditOrigin: state.AuditOrigin,
			SourcePublic: append(ed25519.PublicKey(nil), state.AuditSourcePublic...)}}
}

func validateProvisionStatic(c ProvisionConfig) error {
	for _, dir := range []string{c.AuditQueueDirectory, c.QueueDirectory} {
		if !privateDirectory(dir) || !durableDirectory(dir) {
			return ErrProvision
		}
	}
	if !privateDirectory(c.RuntimeDirectory) || !tmpfsDirectory(c.RuntimeDirectory) {
		return ErrProvision
	}
	info, err := os.Stat(c.RcloneBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return ErrProvision
	}
	if _, err := security.LoadGatewayCentralPin(c.CentralPinFile); err != nil {
		return ErrProvision
	}
	cert, err := security.ReadProtectedGatewayFile(c.CertificateFile, 32<<10)
	if err != nil {
		return ErrProvision
	}
	key, err := security.ReadProtectedGatewayFile(c.TLSKeyFile, 32<<10)
	if err != nil {
		clear(cert)
		return ErrProvision
	}
	defer clear(cert)
	defer clear(key)
	if _, err = tls.X509KeyPair(cert, key); err != nil {
		return ErrProvision
	}
	ca, err := security.ReadProtectedGatewayFile(c.ClientCAFile, 64<<10)
	if err != nil {
		return ErrProvision
	}
	defer clear(ca)
	if !x509.NewCertPool().AppendCertsFromPEM(ca) {
		return ErrProvision
	}
	if c.MaterialSocket == c.AuthoritySocket {
		return ErrProvision
	}
	for _, socket := range []string{c.MaterialSocket, c.AuthoritySocket} {
		if len(socket) > 107 || filepath.Dir(socket) == "/" {
			return ErrProvision
		}
	}
	return nil
}

func privateDirectory(path string) bool {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 ||
		info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func durableDirectory(path string) bool {
	var fs syscall.Statfs_t
	return syscall.Statfs(path, &fs) == nil && fs.Type != 0x01021994 && fs.Type != 0x858458f6
}

func tmpfsDirectory(path string) bool {
	var fs syscall.Statfs_t
	return syscall.Statfs(path, &fs) == nil && fs.Type == 0x01021994
}

type serviceStartup struct {
	Version         int                                  `json:"version"`
	Binding         security.GatewayAuthorizationBinding `json:"binding"`
	SourcePublic    ed25519.PublicKey                    `json:"source_public"`
	DecisionID      uuid.UUID                            `json:"decision_id"`
	LifetimeSeconds int64                                `json:"lifetime_seconds"`
	SocketPath      string                               `json:"socket_path"`
	GatewayUID      uint32                               `json:"gateway_uid"`
	SharedGroup     uint32                               `json:"shared_group"`
}

type preparedPlan struct {
	Version     int             `json:"version"`
	Preparation json.RawMessage `json:"preparation"`
	Startup     serviceStartup  `json:"startup"`
}

type registrationResult struct {
	Version         int                        `json:"version"`
	AuditOrigin     domain.GatewayAuditBinding `json:"audit_origin"`
	SourcePublic    ed25519.PublicKey          `json:"source_public"`
	RecipientPublic []byte                     `json:"recipient_public"`
	CreatedAt       time.Time                  `json:"created_at"`
}

// Finalize binds a fresh local runtime to the central plan and registration
// result and writes the protected service.json. It verifies IDs, public keys,
// sockets and UID/group; it does not register, initialize or start anything.
func Finalize(config ProvisionConfig, statePath, planPath, registrationPath, outputPath string) error {
	for _, p := range []string{statePath, planPath, registrationPath, outputPath} {
		if p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return ErrFinalize
		}
	}
	if statePath == planPath || statePath == registrationPath || statePath == outputPath ||
		planPath == registrationPath || planPath == outputPath || registrationPath == outputPath {
		return ErrFinalize
	}
	state, err := LoadProvisionState(statePath)
	if err != nil || !state.Config.validate() ||
		!bytes.Equal(mustJSON(state.Config), mustJSON(config)) {
		return ErrFinalize
	}
	plan, err := loadPreparedPlan(planPath)
	if err != nil {
		return ErrFinalize
	}
	registration, err := loadRegistrationResult(registrationPath)
	if err != nil {
		return ErrFinalize
	}
	if plan.Startup.Version != 1 || plan.Startup.Binding.RuntimeID != state.RuntimeID ||
		plan.Startup.Binding.AgentID != state.Config.AgentID ||
		!bytes.Equal(plan.Startup.SourcePublic, state.RepositorySourcePublic) ||
		plan.Startup.SocketPath != state.Config.MaterialSocket || plan.Startup.GatewayUID != state.Config.GatewayUID ||
		plan.Startup.SharedGroup != state.Config.SharedGroup ||
		registration.AuditOrigin != state.AuditOrigin || !bytes.Equal(registration.SourcePublic, state.AuditSourcePublic) ||
		len(registration.RecipientPublic) != 32 || bytes.Equal(registration.RecipientPublic, make([]byte, 32)) {
		return ErrFinalize
	}
	service := ServiceConfig{Version: 1, Environment: state.Config.Environment,
		CentralPinFile: state.Config.CentralPinFile, AuditOrigin: state.AuditOrigin,
		AuditSourceFile: state.Config.AuditSourceFile, RecipientPublic: bytes.Clone(registration.RecipientPublic),
		AuditQueueDirectory: state.Config.AuditQueueDirectory, MaxBytes: state.Config.MaxBytes,
		MaxRecords: state.Config.MaxRecords, RuntimeDirectory: state.Config.RuntimeDirectory,
		RcloneBinary: state.Config.RcloneBinary, CertificateFile: state.Config.CertificateFile,
		TLSKeyFile: state.Config.TLSKeyFile, ClientCAFile: state.Config.ClientCAFile,
		ListenAddress: state.Config.ListenAddress, MaxSessions: state.Config.MaxSessions,
		ServerUID: state.Config.ServerUID, SharedGroup: state.Config.SharedGroup,
		ReplaySocket: state.Config.ReplaySocket, StartupWaitSeconds: state.Config.StartupWaitSeconds,
		Repositories: []ServiceRepository{{Binding: plan.Startup.Binding, SourceFile: state.Config.RepositorySourceFile,
			QueueDirectory: state.Config.QueueDirectory, MaterialSocket: state.Config.MaterialSocket,
			AuthoritySocket: state.Config.AuthoritySocket}}}
	if service.Validate() != nil {
		return ErrFinalize
	}
	raw, err := json.Marshal(service)
	if err != nil || persistExactProtected(outputPath, raw) != nil {
		return ErrFinalize
	}
	return nil
}

// persistExactProtected never replaces a different protected file. Exact
// retries remain idempotent; a stale binding/config requires explicit
// operator action instead of silent overwrite.
func persistExactProtected(path string, raw []byte) error {
	if _, err := os.Lstat(path); err == nil {
		existing, readErr := security.ReadProtectedGatewayFile(path, 64<<10)
		if readErr != nil {
			return ErrFinalize
		}
		defer clear(existing)
		if !bytes.Equal(existing, raw) {
			return ErrFinalize
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrFinalize
	}
	return security.PersistProtectedGatewayFile(path, raw)
}

func ensurePrivateDirectory(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrProvision
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return ErrProvision
	}
	return nil
}

func loadPreparedPlan(path string) (preparedPlan, error) {
	raw, err := security.ReadProtectedGatewayFile(path, int64(maxPlan))
	if err != nil {
		return preparedPlan{}, ErrFinalize
	}
	defer clear(raw)
	var p preparedPlan
	if json.Unmarshal(raw, &p) != nil || p.Version != 2 || len(p.Preparation) == 0 ||
		p.Startup.Version != 1 || p.Startup.Binding.Validate() != nil ||
		len(p.Startup.SourcePublic) != ed25519.PublicKeySize ||
		p.Startup.DecisionID.Version() != 7 || p.Startup.LifetimeSeconds < 0 {
		return preparedPlan{}, ErrFinalize
	}
	canonical, err := json.Marshal(p)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return preparedPlan{}, ErrFinalize
	}
	return p, nil
}

func loadRegistrationResult(path string) (registrationResult, error) {
	raw, err := security.ReadProtectedGatewayFile(path, int64(maxRegistr))
	if err != nil {
		return registrationResult{}, ErrFinalize
	}
	defer clear(raw)
	var r registrationResult
	if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.AuditOrigin.Validate() != nil ||
		len(r.SourcePublic) != ed25519.PublicKeySize || len(r.RecipientPublic) != 32 || r.CreatedAt.IsZero() {
		return registrationResult{}, ErrFinalize
	}
	canonical, err := json.Marshal(r)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return registrationResult{}, ErrFinalize
	}
	return r, nil
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}
