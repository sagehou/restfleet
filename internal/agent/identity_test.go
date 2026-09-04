package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/security"
)

func TestStateVolumePreservesInstallIdentityAndPrivateKey(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "agent-state")
	first, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	installID, err := first.InstallID()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := LoadOrCreatePrivateKey(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenState(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	reopenedID, err := second.InstallID()
	if err != nil {
		t.Fatal(err)
	}
	reopenedKey, err := LoadOrCreatePrivateKey(second)
	if err != nil {
		t.Fatal(err)
	}
	if installID != reopenedID || !bytes.Equal(privateKey, reopenedKey) {
		t.Fatal("retained state volume changed the Agent install identity")
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{
		{directory, 0o700},
		{filepath.Join(directory, "state.db"), 0o600},
		{filepath.Join(directory, privateKeyFile), 0o600},
	} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.mode {
			t.Fatalf("%s mode = %o, want %o", check.path, info.Mode().Perm(), check.mode)
		}
	}
}

func TestCreateCSRNeverContainsPrivateKey(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	privateKey, err := LoadOrCreatePrivateKey(state)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(csr, []byte("PRIVATE KEY")) {
		t.Fatal("CSR contains private key material")
	}
}
func TestSaveRotationValidatesAndReplacesIdentity(t *testing.T) {
	state, err := OpenState(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	privateKey, err := LoadOrCreatePrivateKey(state)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := CreateCSR(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ca, caPrivatePEM, err := security.NewAgentCA(now)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(caPrivatePEM)
	agentID := uuid.Must(uuid.NewV7())
	initial, err := ca.IssueAgentCertificate(csr, agentID, now)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{
		AgentID: agentID, HostID: uuid.Must(uuid.NewV7()),
		CertificatePEM: string(initial.CertificatePEM), CABundlePEM: string(ca.CertificatePEM()),
		NotAfter: initial.NotAfter, ServerName: "control.example", GRPCEndpoint: "control.example:443",
	}
	if err := SaveIdentity(state, identity); err != nil {
		t.Fatal(err)
	}
	rotated, err := ca.IssueAgentCertificate(csr, agentID, now.Add(23*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	response := &agentv1.ServerToAgent{
		Payload: &agentv1.ServerToAgent_CertificateRotationResponse{
			CertificateRotationResponse: &agentv1.CertificateRotationResponse{
				CertificatePem: string(rotated.CertificatePEM), CaBundlePem: string(ca.CertificatePEM()),
				NotAfter: timestamppb.New(rotated.NotAfter),
			},
		},
	}
	if err := saveRotation(state, identity, response); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadIdentity(state)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CertificatePEM != string(rotated.CertificatePEM) || !stored.NotAfter.Equal(rotated.NotAfter) {
		t.Fatal("rotated identity was not atomically persisted")
	}
	otherCertificate, err := ca.IssueAgentCertificate(csr, uuid.Must(uuid.NewV7()), now.Add(24*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	response.Payload = &agentv1.ServerToAgent_CertificateRotationResponse{
		CertificateRotationResponse: &agentv1.CertificateRotationResponse{
			CertificatePem: string(otherCertificate.CertificatePEM), CaBundlePem: string(ca.CertificatePEM()),
			NotAfter: timestamppb.New(otherCertificate.NotAfter),
		},
	}
	if err := saveRotation(state, stored, response); err == nil {
		t.Fatal("certificate for another Agent replaced the local identity")
	}
	unchanged, err := LoadIdentity(state)
	if err != nil || unchanged.CertificatePEM != stored.CertificatePEM {
		t.Fatal("rejected rotation changed the saved identity")
	}
}
