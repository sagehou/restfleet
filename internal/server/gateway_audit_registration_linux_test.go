package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/domain"
)

func TestGatewayAuditRegistrationProtectedCanonicalMetadata(t *testing.T) {
	s := GatewayAuditRegistrationConfig{Version: 1, AuditOrigin: domain.GatewayAuditBinding{
		OriginID: uuid.Must(uuid.NewV7()), RuntimeID: uuid.Must(uuid.NewV7())}, SourcePublic: bytes.Repeat([]byte{3}, 32)}
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory")
	}
	path := filepath.Join(dir, "audit.json")
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", "  ") != nil || os.WriteFile(path, pretty.Bytes(), 0400) != nil {
		t.Fatal("metadata fixture")
	}
	loaded, err := LoadGatewayAuditRegistrationConfig(path)
	if err != nil || loaded.AuditOrigin != s.AuditOrigin || !bytes.Equal(loaded.SourcePublic, s.SourcePublic) {
		t.Fatal("protected registration metadata not loaded")
	}
	if os.Chmod(path, 0600) != nil {
		t.Fatal("writable fixture")
	}
	for _, bad := range [][]byte{
		append(bytes.Clone(raw), raw...),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"Version":1`), 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":1,"private-secret-canary":true`), 1),
		bytes.Replace(raw, []byte(`"version":1,`), nil, 1),
		bytes.Replace(raw, []byte(`"version":1`), []byte(`"version":null`), 1),
		bytes.Replace(raw, []byte(s.AuditOrigin.OriginID.String()), []byte(uuid.Nil.String()), 1),
		bytes.Replace(raw, []byte(s.AuditOrigin.RuntimeID.String()), []byte(uuid.Nil.String()), 1),
		bytes.Replace(raw, []byte(`"source_public":`), []byte(`"source_public":null,"ignored":`), 1),
		[]byte("private-config-canary"), bytes.Repeat([]byte{' '}, 1025),
	} {
		if os.WriteFile(path, bad, 0600) != nil {
			t.Fatal("invalid metadata fixture")
		}
		if got, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration || got.SourcePublic != nil || got.AuditOrigin.OriginID != uuid.Nil {
			t.Fatal("ambiguous metadata loaded or unsafe error/output")
		}
	}
	for _, source := range [][]byte{nil, make([]byte, 32), bytes.Repeat([]byte{3}, 64)} {
		bad := s
		bad.SourcePublic = source
		rawBad, err := json.Marshal(bad)
		if err != nil || os.WriteFile(path, rawBad, 0600) != nil {
			t.Fatal("invalid key fixture")
		}
		if _, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration {
			t.Fatal("invalid source public key accepted")
		}
	}
	if os.WriteFile(path, raw, 0600) != nil || os.Chmod(path, 0644) != nil {
		t.Fatal("unsafe file fixture")
	}
	if _, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration {
		t.Fatal("unsafe metadata permissions accepted")
	}
	if os.Chmod(path, 0600) != nil || os.Chmod(dir, 0755) != nil {
		t.Fatal("unsafe directory fixture")
	}
	if _, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration {
		t.Fatal("nonprivate metadata directory accepted")
	}
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("directory reset")
	}
	link := filepath.Join(dir, "link.json")
	if os.Symlink(path, link) != nil {
		t.Fatal("symlink fixture")
	}
	if _, err := LoadGatewayAuditRegistrationConfig(link); err != ErrGatewayAuditRegistration {
		t.Fatal("symlink followed")
	}
	if os.Link(path, link+".hard") != nil {
		t.Fatal("hardlink fixture")
	}
	if _, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration {
		t.Fatal("hardlinked metadata accepted")
	}
	if os.Remove(link+".hard") != nil || os.WriteFile(path+".pending", nil, 0600) != nil {
		t.Fatal("pending fixture")
	}
	if _, err := LoadGatewayAuditRegistrationConfig(path); err != ErrGatewayAuditRegistration {
		t.Fatal("uncertain pending metadata accepted")
	}
}
