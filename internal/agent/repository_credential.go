package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/domain"
)

const repositoryCredentialFile = "repository-credential.json"

// Separate 0600 file, not a generic desired-state/log/outbound record. V1 has
// one Repository per Host; a single atomic file avoids cross-file commit gaps.
func (s *State) ApplyRepositoryCredential(identity Identity, message *agentv1.CredentialRevision) (*agentv1.CredentialRevisionAccepted, error) {
	if message == nil {
		return nil, domain.ErrRepositoryCredential
	}
	defer clear(message.GatewayPassword)
	defer clear(message.ResticPassword)
	c, err := repositoryCredentialFromProto(message)
	if err != nil || c.AgentID != identity.AgentID || c.HostID != identity.HostID {
		return nil, domain.ErrRepositoryCredential
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if err := s.checkCredentialDirectory(); err != nil {
		return nil, err
	}
	previous, found, err := s.loadRepositoryCredential()
	if err != nil {
		return nil, err
	}
	defer previous.Clear()
	encoded, err := json.Marshal(c)
	if err != nil {
		return nil, domain.ErrRepositoryCredential
	}
	defer clear(encoded)
	if found {
		if previous.AgentID != c.AgentID || previous.HostID != c.HostID || previous.RepositoryID != c.RepositoryID || previous.GatewayID != c.GatewayID ||
			c.Revision < previous.Revision || c.GatewayRevision < previous.GatewayRevision || c.ResticRevision < previous.ResticRevision {
			return nil, domain.ErrRepositoryCredential
		}
		if c.Revision == previous.Revision {
			old, err := json.Marshal(previous)
			if err != nil {
				return nil, domain.ErrRepositoryCredential
			}
			same := bytes.Equal(encoded, old)
			clear(old)
			if !same {
				return nil, domain.ErrRepositoryCredential
			}
		} else if c.DeliveryID == previous.DeliveryID ||
			(c.GatewayRevision == previous.GatewayRevision && !bytes.Equal(c.GatewayPassword, previous.GatewayPassword)) ||
			(c.ResticRevision == previous.ResticRevision && !bytes.Equal(c.ResticPassword, previous.ResticPassword)) {
			return nil, domain.ErrRepositoryCredential
		}
	}
	// Even a duplicate fsyncs before ACK, including after a previous directory
	// fsync error. Never acknowledge solely because rename made a file visible.
	if err := atomicWrite(filepath.Join(s.Directory(), repositoryCredentialFile), encoded, 0o600); err != nil {
		return nil, domain.ErrRepositoryCredential
	}
	return &agentv1.CredentialRevisionAccepted{DeliveryId: c.DeliveryID.String(), Revision: c.Revision}, nil
}

func (s *State) LoadRepositoryCredential(identity Identity) (domain.RepositoryCredential, bool, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	if err := s.checkCredentialDirectory(); err != nil {
		return domain.RepositoryCredential{}, false, err
	}
	c, found, err := s.loadRepositoryCredential()
	if err != nil || (found && (c.AgentID != identity.AgentID || c.HostID != identity.HostID)) {
		c.Clear()
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	return c, found, nil
}

func (s *State) checkCredentialDirectory() error {
	dir := s.Directory()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || resolved != dir {
		return domain.ErrRepositoryCredential
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return domain.ErrRepositoryCredential
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return domain.ErrRepositoryCredential
	}
	return nil
}

func (s *State) loadRepositoryCredential() (domain.RepositoryCredential, bool, error) {
	path := filepath.Join(s.Directory(), repositoryCredentialFile)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return domain.RepositoryCredential{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > 128<<10 {
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	raw, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	defer clear(raw)
	if err != nil || len(raw) > 128<<10 {
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	var c domain.RepositoryCredential
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&c) != nil || decoder.Decode(new(any)) != io.EOF || c.Validate() != nil {
		c.Clear()
		return domain.RepositoryCredential{}, false, domain.ErrRepositoryCredential
	}
	return c, true, nil
}

func repositoryCredentialFromProto(m *agentv1.CredentialRevision) (domain.RepositoryCredential, error) {
	c := domain.RepositoryCredential{Revision: m.GetRevision(), GatewayRevision: m.GetGatewayRevision(), ResticRevision: m.GetResticRevision(),
		Endpoint: m.GetEndpoint(), CABundlePEM: m.GetCaBundlePem(), GatewayPassword: m.GetGatewayPassword(), ResticPassword: m.GetResticPassword()}
	for _, field := range []struct {
		raw string
		dst *uuid.UUID
	}{{m.GetDeliveryId(), &c.DeliveryID}, {m.GetAgentId(), &c.AgentID}, {m.GetHostId(), &c.HostID}, {m.GetRepositoryId(), &c.RepositoryID}, {m.GetGatewayId(), &c.GatewayID}} {
		id, err := uuid.Parse(field.raw)
		if err != nil || id.String() != field.raw {
			return c, domain.ErrRepositoryCredential
		}
		*field.dst = id
	}
	if m.GetValidFrom() == nil || m.GetValidFrom().CheckValid() != nil {
		return c, domain.ErrRepositoryCredential
	}
	c.ValidFrom = m.GetValidFrom().AsTime().UTC()
	return c, c.Validate()
}
