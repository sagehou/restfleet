package agent

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/security"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func credentialFixture(t *testing.T) (Identity, *agentv1.CredentialRevision) {
	t.Helper()
	ca, key, err := security.NewAgentCA(time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	id := func() string { return uuid.Must(uuid.NewV7()).String() }
	m := &agentv1.CredentialRevision{DeliveryId: id(), AgentId: id(), HostId: id(), RepositoryId: id(), GatewayId: id(), Revision: 1, GatewayRevision: 1, ResticRevision: 1,
		CaBundlePem: ca.CertificatePEM(), GatewayPassword: []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{41}, 32))),
		ResticPassword: []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))), ValidFrom: timestamppb.Now()}
	m.Endpoint = "https://gateway.example/restic/" + m.GatewayId + "/" + m.RepositoryId + "/"
	return Identity{AgentID: uuid.MustParse(m.AgentId), HostID: uuid.MustParse(m.HostId)}, m
}

func TestRepositoryCredentialDurableReplayAndOfflineRead(t *testing.T) {
	identity, m := credentialFixture(t)
	dir := t.TempDir()
	s, err := OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	message := proto.Clone(m).(*agentv1.CredentialRevision)
	ack, err := s.ApplyRepositoryCredential(identity, message)
	if err != nil || ack.GetDeliveryId() != m.DeliveryId || ack.GetRevision() != 1 {
		t.Fatal("durable credential not acknowledged")
	}
	if !bytes.Equal(message.GatewayPassword, make([]byte, 43)) || !bytes.Equal(message.ResticPassword, make([]byte, 43)) {
		t.Fatal("received secrets not cleared")
	}
	info, err := os.Stat(filepath.Join(dir, repositoryCredentialFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("credential permissions")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(db, m.GatewayPassword) || bytes.Contains(db, m.ResticPassword) || bytes.Contains(db, []byte(base64.StdEncoding.EncodeToString(m.GatewayPassword))) {
		t.Fatal("bbolt contains repository secrets")
	}
	s, err = OpenState(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, ok, err := s.LoadRepositoryCredential(identity)
	if err != nil || !ok || !bytes.Equal(c.ResticPassword, m.ResticPassword) {
		t.Fatal("credential lost across restart/offline read")
	}
	c.Clear()
	if _, err := s.ApplyRepositoryCredential(identity, proto.Clone(m).(*agentv1.CredentialRevision)); err != nil {
		t.Fatal("ACK loss replay failed")
	}
	// A later delivery may change endpoint while retaining the same key versions.
	next := proto.Clone(m).(*agentv1.CredentialRevision)
	next.Revision, next.DeliveryId = 2, uuid.Must(uuid.NewV7()).String()
	next.Endpoint = strings.Replace(next.Endpoint, "gateway.example", "new-gateway.example", 1)
	if _, err := s.ApplyRepositoryCredential(identity, next); err != nil {
		t.Fatal("next revision failed")
	}
	if _, err := s.ApplyRepositoryCredential(identity, proto.Clone(m).(*agentv1.CredentialRevision)); err == nil {
		t.Fatal("rollback accepted")
	}
	c, ok, err = s.LoadRepositoryCredential(identity)
	if err != nil || !ok || c.Revision != 2 {
		t.Fatal("rejection lost last known good")
	}
	c.Clear()
}

func TestRepositoryCredentialValidationAndIsolation(t *testing.T) {
	identity, m := credentialFixture(t)
	for name, change := range map[string]func(*agentv1.CredentialRevision){
		"other Host":        func(m *agentv1.CredentialRevision) { m.HostId = uuid.Must(uuid.NewV7()).String() },
		"other Agent":       func(m *agentv1.CredentialRevision) { m.AgentId = uuid.Must(uuid.NewV7()).String() },
		"wrong path":        func(m *agentv1.CredentialRevision) { m.RepositoryId = uuid.Must(uuid.NewV7()).String() },
		"noncanonical UUID": func(m *agentv1.CredentialRevision) { m.DeliveryId = "{" + m.DeliveryId + "}" },
		"http":              func(m *agentv1.CredentialRevision) { m.Endpoint = strings.Replace(m.Endpoint, "https:", "http:", 1) },
		"userinfo": func(m *agentv1.CredentialRevision) {
			m.Endpoint = strings.Replace(m.Endpoint, "https://", "https://secret@", 1)
		},
		"query": func(m *agentv1.CredentialRevision) { m.Endpoint += "?" },
		"traversal": func(m *agentv1.CredentialRevision) {
			m.Endpoint = strings.Replace(m.Endpoint, "/restic/", "/../restic/", 1)
		},
		"invalid CA":         func(m *agentv1.CredentialRevision) { m.CaBundlePem = []byte("test-private-key-canary") },
		"CA trailing junk":   func(m *agentv1.CredentialRevision) { m.CaBundlePem = append(m.CaBundlePem, []byte("junk")...) },
		"same passwords":     func(m *agentv1.CredentialRevision) { m.GatewayPassword = bytes.Clone(m.ResticPassword) },
		"password injection": func(m *agentv1.CredentialRevision) { m.ResticPassword = []byte("password\nRCLONE_CONFIG=canary") },
		"bad version":        func(m *agentv1.CredentialRevision) { m.ResticRevision = 0 },
		"timestamp":          func(m *agentv1.CredentialRevision) { m.ValidFrom = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s, err := OpenState(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			next := proto.Clone(m).(*agentv1.CredentialRevision)
			change(next)
			if _, err := s.ApplyRepositoryCredential(identity, next); err == nil {
				t.Fatal("unsafe credential accepted")
			}
			if _, err := os.Stat(filepath.Join(s.Directory(), repositoryCredentialFile)); !os.IsNotExist(err) {
				t.Fatal("rejection wrote secret")
			}
		})
	}
	for name, change := range map[string]func(*agentv1.CredentialRevision){
		"same revision conflict": func(m *agentv1.CredentialRevision) { m.DeliveryId = uuid.Must(uuid.NewV7()).String() },
		"password without key revision": func(m *agentv1.CredentialRevision) {
			m.Revision++
			m.DeliveryId = uuid.Must(uuid.NewV7()).String()
			m.GatewayPassword = []byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{43}, 32)))
		},
		"repository replacement": func(m *agentv1.CredentialRevision) {
			m.Revision++
			m.DeliveryId = uuid.Must(uuid.NewV7()).String()
			id := uuid.Must(uuid.NewV7()).String()
			m.Endpoint = strings.Replace(m.Endpoint, m.RepositoryId, id, 1)
			m.RepositoryId = id
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := OpenState(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.ApplyRepositoryCredential(identity, proto.Clone(m).(*agentv1.CredentialRevision)); err != nil {
				t.Fatal(err)
			}
			next := proto.Clone(m).(*agentv1.CredentialRevision)
			change(next)
			if _, err := s.ApplyRepositoryCredential(identity, next); err == nil {
				t.Fatal("conflicting revision accepted")
			}
			c, ok, err := s.LoadRepositoryCredential(identity)
			if err != nil || !ok || c.Revision != 1 {
				t.Fatal("last known good lost")
			}
			c.Clear()
		})
	}
}

func TestRepositoryCredentialRejectsUnsafeFilesAndConcurrentRollback(t *testing.T) {
	identity, m := credentialFixture(t)
	for _, mode := range []string{"permissions", "symlink", "directory"} {
		t.Run(mode, func(t *testing.T) {
			s, err := OpenState(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			path := filepath.Join(s.Directory(), repositoryCredentialFile)
			switch mode {
			case "permissions":
				err = os.WriteFile(path, []byte("canary"), 0o644)
			case "symlink":
				err = os.Symlink(filepath.Join(s.Directory(), "state.db"), path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.ApplyRepositoryCredential(identity, proto.Clone(m).(*agentv1.CredentialRevision)); err == nil {
				t.Fatal("unsafe credential file accepted")
			}
		})
	}
	s, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for revision := int64(1); revision <= 8; revision++ {
		next := proto.Clone(m).(*agentv1.CredentialRevision)
		next.Revision = revision
		next.DeliveryId = uuid.Must(uuid.NewV7()).String()
		wg.Go(func() { _, _ = s.ApplyRepositoryCredential(identity, next) })
	}
	wg.Wait()
	c, ok, err := s.LoadRepositoryCredential(identity)
	if err != nil || !ok || c.Revision != 8 {
		t.Fatal("concurrent write rolled back newest revision")
	}
	c.Clear()
}

func TestInventoryAdvertisesRepositoryCredentials(t *testing.T) {
	s, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Contains(inventorySnapshot(s, "test", 0).GetCapabilities(), domain.RepositoryCredentialsCapability) {
		t.Fatal("inventory and Hello capabilities disagree")
	}
}
