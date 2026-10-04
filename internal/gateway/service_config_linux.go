package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/security"
)

var ErrService = errors.New("gateway service unavailable or inconsistent")

// ServiceConfig is protected Gateway-local metadata for ONE fresh process.
// It contains paths/public keys only; it cannot prove central registration or
// process freshness, replace an admission, restore an owner or release a fence.
type ServiceConfig struct {
	Version             int                          `json:"version"`
	Environment         string                       `json:"environment"`
	CentralPinFile      string                       `json:"central_pin_file"`
	AuditOrigin         security.GatewayAuditBinding `json:"audit_origin"`
	AuditSourceFile     string                       `json:"audit_source_file"`
	RecipientPublic     []byte                       `json:"recipient_public"`
	AuditQueueDirectory string                       `json:"audit_queue_directory"`
	MaxBytes            int64                        `json:"max_bytes"`
	MaxRecords          int                          `json:"max_records"`
	RuntimeDirectory    string                       `json:"runtime_directory"`
	RcloneBinary        string                       `json:"rclone_binary"`
	CertificateFile     string                       `json:"certificate_file"`
	TLSKeyFile          string                       `json:"tls_key_file"`
	ListenAddress       string                       `json:"listen_address"`
	MaxSessions         int                          `json:"max_sessions"`
	ServerUID           uint32                       `json:"server_uid"`
	SharedGroup         uint32                       `json:"shared_group"`
	ReplaySocket        string                       `json:"replay_socket"`
	StartupWaitSeconds  int                          `json:"startup_wait_seconds"`
	Repositories        []ServiceRepository          `json:"repositories"`
}

type ServiceRepository struct {
	Binding         security.GatewayAuthorizationBinding `json:"binding"`
	SourceFile      string                               `json:"source_file"`
	QueueDirectory  string                               `json:"queue_directory"`
	MaterialSocket  string                               `json:"material_socket"`
	AuthoritySocket string                               `json:"authority_socket"`
}

func (s ServiceConfig) Validate() error {
	if s.Version != 1 || (s.Environment != "production" && s.Environment != "development" && s.Environment != "test") ||
		s.AuditOrigin.Validate() != nil || len(s.RecipientPublic) != 32 || bytes.Equal(s.RecipientPublic, make([]byte, 32)) ||
		s.MaxBytes < security.MaxGatewayPendingSize+(16<<10) || s.MaxBytes > 64<<20 || s.MaxRecords < 1 || s.MaxRecords > 4096 ||
		s.MaxSessions < 1 || s.MaxSessions > 32 || len(s.Repositories) < 1 || len(s.Repositories) > 32 ||
		s.StartupWaitSeconds < 1 || s.StartupWaitSeconds > 300 || s.ServerUID == ^uint32(0) || s.SharedGroup == ^uint32(0) {
		return ErrService
	}
	uid := uint32(os.Geteuid())
	if (s.SharedGroup == 0 && s.ServerUID != uid) ||
		(s.SharedGroup != 0 && (uid == 0 || s.ServerUID == 0 || s.ServerUID == uid)) ||
		(s.Environment == "production" && (uid == 0 || s.SharedGroup == 0)) {
		return ErrService
	}
	host, port, err := net.SplitHostPort(s.ListenAddress)
	n, numberErr := strconv.ParseUint(port, 10, 16)
	if err != nil || numberErr != nil || net.ParseIP(host) == nil || strconv.FormatUint(n, 10) != port ||
		net.JoinHostPort(host, port) != s.ListenAddress || (s.Environment == "production" && n == 0) {
		return ErrService
	}
	paths := []string{s.CentralPinFile, s.AuditSourceFile, s.AuditQueueDirectory, s.RuntimeDirectory, s.RcloneBinary, s.CertificateFile, s.TLSKeyFile, s.ReplaySocket}
	privateDirs := []string{s.AuditQueueDirectory, s.RuntimeDirectory}
	keyDirs := []string{filepath.Dir(s.CentralPinFile), filepath.Dir(s.AuditSourceFile), filepath.Dir(s.CertificateFile), filepath.Dir(s.TLSKeyFile)}
	ipcDirs := []string{filepath.Dir(s.ReplaySocket)}
	var ids [4]map[uuid.UUID]bool
	for i := range ids {
		ids[i] = make(map[uuid.UUID]bool)
	}
	for _, r := range s.Repositories {
		if r.Binding.Validate() != nil || r.Binding.RuntimeID != s.AuditOrigin.RuntimeID {
			return ErrService
		}
		for i, id := range []uuid.UUID{r.Binding.AdmissionID, r.Binding.HostID, r.Binding.RepositoryID, r.Binding.GatewayID} {
			if ids[i][id] {
				return ErrService
			}
			ids[i][id] = true
		}
		paths = append(paths, r.SourceFile, r.QueueDirectory, r.MaterialSocket, r.AuthoritySocket)
		privateDirs = append(privateDirs, r.QueueDirectory)
		keyDirs = append(keyDirs, filepath.Dir(r.SourceFile))
		ipcDirs = append(ipcDirs, filepath.Dir(r.MaterialSocket), filepath.Dir(r.AuthoritySocket))
		if len(r.MaterialSocket) > 107 || len(r.AuthoritySocket) > 107 {
			return ErrService
		}
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.IndexFunc(path, unicode.IsControl) >= 0 || seen[path] {
			return ErrService
		}
		seen[path] = true
	}
	if len(s.ReplaySocket) > 107 {
		return ErrService
	}
	for i, dir := range privateDirs {
		for _, other := range privateDirs[i+1:] {
			if servicePathsOverlap(dir, other) {
				return ErrService
			}
		}
		for _, other := range append(append([]string(nil), keyDirs...), ipcDirs...) {
			if servicePathsOverlap(dir, other) {
				return ErrService
			}
		}
	}
	for _, dir := range keyDirs {
		for _, ipc := range ipcDirs {
			if servicePathsOverlap(dir, ipc) {
				return ErrService
			}
		}
	}
	return nil
}

func servicePathsOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func LoadServiceConfig(path string) (ServiceConfig, error) {
	raw, err := security.ReadProtectedGatewayFile(path, 64<<10)
	if err != nil {
		return ServiceConfig{}, ErrService
	}
	defer clear(raw)
	var s ServiceConfig
	if json.Unmarshal(raw, &s) != nil || s.Validate() != nil {
		return ServiceConfig{}, ErrService
	}
	canonical, err := json.Marshal(s)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(canonical, compact.Bytes()) {
		return ServiceConfig{}, ErrService
	}
	for _, dir := range []string{s.RuntimeDirectory, s.AuditQueueDirectory} {
		if servicePathsOverlap(filepath.Dir(path), dir) {
			return ServiceConfig{}, ErrService
		}
	}
	for _, r := range s.Repositories {
		if servicePathsOverlap(filepath.Dir(path), r.QueueDirectory) || servicePathsOverlap(filepath.Dir(path), filepath.Dir(r.MaterialSocket)) ||
			servicePathsOverlap(filepath.Dir(path), filepath.Dir(r.AuthoritySocket)) {
			return ServiceConfig{}, ErrService
		}
	}
	if servicePathsOverlap(filepath.Dir(path), filepath.Dir(s.ReplaySocket)) {
		return ServiceConfig{}, ErrService
	}
	return s, nil
}

// Queue storage must persist across loss of the process/tmpfs. This check is a
// startup precondition; encrypted Queue retains its own inode/ownership/fsync
// checks. Provisioning must provide durable storage and trusted ancestors.
func serviceDurableDirectory(path string) bool {
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
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	var fs syscall.Statfs_t
	return ok && stat.Uid == uint32(os.Geteuid()) && syscall.Fstatfs(int(f.Fd()), &fs) == nil && fs.Type != 0x01021994 && fs.Type != 0x858458f6
}
