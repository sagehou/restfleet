package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/gatewaypending"
	"github.com/sagehou/restfleet/internal/security"
)

func TestServiceConfigRejectsAmbiguousMetadataAndUnsafeBoundaries(t *testing.T) {
	f := newServiceFixture(t, 2, "success")
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil { t.Fatal("config private directory") }
	path := filepath.Join(dir, "service.json")
	raw, err := json.Marshal(f.config)
	if err != nil { t.Fatal(err) }
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil || os.WriteFile(path, pretty.Bytes(), 0600) != nil { t.Fatal("pretty metadata fixture") }
	if _, err := LoadServiceConfig(path); err != nil { t.Fatal("formatted metadata rejected") }
	for _, bad := range [][]byte{
		append(bytes.Clone(raw), raw...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"Version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"private-secret-canary":true`), 1),
		bytes.Replace(raw, []byte(`"version":1,`), nil, 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":null`), 1),
		bytes.Replace(raw, []byte(`"shared_group":0`), []byte(`"shared_group":null`), 1),
		bytes.Repeat([]byte{' '}, (64<<10)+1),
	} {
		if os.WriteFile(path, bad, 0600) != nil { t.Fatal("bad metadata fixture") }
		if got, err := LoadServiceConfig(path); err != ErrService || got.Repositories != nil { t.Fatal("ambiguous metadata accepted or raw error returned") }
	}
	for _, mutate := range []func(*ServiceConfig){
		func(s *ServiceConfig) { s.Environment = "unsafe" },
		func(s *ServiceConfig) { s.Version = 2 },
		func(s *ServiceConfig) { s.AuditOrigin.RuntimeID = uuid.Nil },
		func(s *ServiceConfig) { s.RecipientPublic = make([]byte, 32) },
		func(s *ServiceConfig) { s.MaxBytes = 64<<20 + 1 },
		func(s *ServiceConfig) { s.MaxBytes = 512<<10 },
		func(s *ServiceConfig) { s.MaxRecords = 4097 },
		func(s *ServiceConfig) { s.MaxSessions = 33 },
		func(s *ServiceConfig) { s.StartupWaitSeconds = 301 },
		func(s *ServiceConfig) { s.StartupWaitSeconds = 0 },
		func(s *ServiceConfig) { s.Repositories = nil },
		func(s *ServiceConfig) { s.Repositories = append(s.Repositories, s.Repositories[0]) },
		func(s *ServiceConfig) { s.Repositories[0].Binding.RuntimeID = uuid.Must(uuid.NewV7()) },
		func(s *ServiceConfig) { s.ServerUID = ^uint32(0) },
		func(s *ServiceConfig) { s.SharedGroup = ^uint32(0) },
		func(s *ServiceConfig) { s.ServerUID++ },
		func(s *ServiceConfig) { s.ListenAddress = "localhost:443" },
		func(s *ServiceConfig) { s.ListenAddress = "127.0.0.1:0443" },
		func(s *ServiceConfig) { s.ListenAddress = "127.0.0.1:65536" },
		func(s *ServiceConfig) { s.ReplaySocket = "/run/../unsafe.sock" },
		func(s *ServiceConfig) { s.CentralPinFile += "\n" },
		func(s *ServiceConfig) { s.Repositories[0].QueueDirectory = filepath.Dir(s.AuditSourceFile) },
		func(s *ServiceConfig) { s.Repositories[0].QueueDirectory = filepath.Join(s.AuditQueueDirectory, "nested") },
		func(s *ServiceConfig) { s.Repositories[0].AuthoritySocket = s.Repositories[0].MaterialSocket },
		func(s *ServiceConfig) { s.TLSKeyFile = s.AuditSourceFile },
	} {
		bad := f.config
		bad.Repositories = append([]ServiceRepository(nil), f.config.Repositories...)
		mutate(&bad)
		if bad.Validate() != ErrService { t.Fatal("unsafe service boundary accepted") }
	}
	if os.WriteFile(path, raw, 0600) != nil || os.Chmod(path, 0644) != nil { t.Fatal("permission fixture") }
	if _, err := LoadServiceConfig(path); err != ErrService { t.Fatal("public config accepted") }
	if os.Chmod(path, 0600) != nil || os.WriteFile(path+".pending", nil, 0600) != nil { t.Fatal("pending fixture") }
	if _, err := LoadServiceConfig(path); err != ErrService { t.Fatal("uncertain metadata accepted") }
}

func TestServiceStartupRejectsVolatileQueueAndBadTLSWithoutBindingChannels(t *testing.T) {
	for _, failure := range []string{"volatile-audit", "volatile-repository", "queue-permissions", "source-missing", "tls", "source-key-reuse", "existing-material-socket", "existing-global-queue", "production-root"} {
		t.Run(failure, func(t *testing.T) {
			f := newServiceFixture(t, 1, "success")
			switch failure {
			case "volatile-audit", "volatile-repository":
				path, err := os.MkdirTemp("/dev/shm", "rf-queue-")
				if err != nil { t.Fatal(err) }
				t.Cleanup(func() { _ = os.RemoveAll(path) })
				if failure == "volatile-audit" { f.config.AuditQueueDirectory = path } else { f.config.Repositories[0].QueueDirectory = path }
				if f.config.Validate() != nil { t.Fatal("volatile directory fixture failed before filesystem check") }
			case "tls": if os.WriteFile(f.config.TLSKeyFile, []byte("private-key-canary"), 0600) != nil { t.Fatal("bad key fixture") }
			case "queue-permissions": if os.Chmod(f.config.Repositories[0].QueueDirectory, 0750) != nil { t.Fatal("bad directory fixture") }
			case "source-missing": if os.Remove(f.config.Repositories[0].SourceFile) != nil { t.Fatal("missing source fixture") }
			case "existing-material-socket":
				l, err := gatewaypending.ListenReplay(f.config.Repositories[0].MaterialSocket)
				if err != nil { t.Fatal("old material listener fixture") }
				defer l.Close()
			case "existing-global-queue":
				source, pin, err := security.LoadGatewayTrust(f.config.AuditSourceFile, f.config.CentralPinFile)
				if err != nil { t.Fatal("old source fixture") }
				defer clear(source)
				q, err := gatewaypending.CreateGlobalAudit(f.config.AuditQueueDirectory, f.config.AuditOrigin, [32]byte(f.config.RecipientPublic), source, pin, gatewaypending.Limits{MaxBytes: f.config.MaxBytes, MaxRecords: f.config.MaxRecords})
				if err != nil || q.Close() != nil { t.Fatal("old global queue fixture") }
			case "source-key-reuse":
				raw, err := os.ReadFile(f.config.AuditSourceFile)
				if err != nil || os.WriteFile(f.config.Repositories[0].SourceFile, raw, 0600) != nil { t.Fatal("reused source fixture") }
				clear(raw)
			case "production-root":
				if os.Geteuid() != 0 { t.Skip("root guard exercised in Actions isolation step") }
				f.config.Environment = "production"
				f.config.ServerUID, f.config.SharedGroup = serviceCenterUID, serviceGroup
				f.config.ListenAddress = "127.0.0.1:18443"
			}
			if s, err := StartService(context.Background(), f.config); err != ErrService || s != nil { t.Fatal("unsafe startup accepted") }
			if _, err := os.Lstat(f.config.Repositories[0].MaterialSocket); failure == "existing-material-socket" {
				if err != nil { t.Fatal("startup removed a pre-existing socket") }
			} else if !os.IsNotExist(err) { t.Fatal("unsafe startup bound material channel") }
		})
	}
}
