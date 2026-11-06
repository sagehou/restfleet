package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"github.com/sagehou/restfleet/internal/agent"
	"github.com/sagehou/restfleet/internal/agent/schedule"
	"github.com/sagehou/restfleet/internal/domain"
	"github.com/sagehou/restfleet/internal/rclone"
	centralrestic "github.com/sagehou/restfleet/internal/restic"
	"github.com/sagehou/restfleet/internal/security"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Only the cloud remote is replaced by a local fixture. Scheduler, bbolt,
// Agent mTLS, Gateway owner and both pinned engines are production paths.
func TestPinnedAgentOfflineScheduleAndCentralGatewayIndex(t *testing.T) {
	resticBinary, rcloneBinary := os.Getenv("RESTFLEET_TEST_RESTIC_BINARY"), os.Getenv("RESTFLEET_TEST_RCLONE_BINARY")
	if resticBinary == "" || rcloneBinary == "" {
		t.Skip("CI supplies pinned Restic and rclone")
	}
	if os.Geteuid() == 0 {
		t.Fatal("pinned Agent conformance needs an unprivileged runner to verify unreadable files")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newServiceFixture(t, 1, "real")
	if err := os.WriteFile(filepath.Join(f.state, "real-rclone"), []byte(rcloneBinary), 0600); err != nil {
		t.Fatal(err)
	}
	password, err := security.NewOpaqueToken()
	if err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(f.state, "repository-password")
	if err = os.WriteFile(passwordFile, []byte(password), 0600); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(f.state, "repo")
	init := exec.CommandContext(ctx, resticBinary, "--json", "init", "--repository-version", "2")
	init.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "RESTIC_REPOSITORY=" + repository, "RESTIC_PASSWORD_FILE=" + passwordFile}
	init.Stdout, init.Stderr = io.Discard, io.Discard
	if err = init.Run(); err != nil {
		t.Fatal("pinned fixture initialization failed")
	}
	ca, key, err := security.NewAgentCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	caPath := filepath.Join(filepath.Dir(f.config.CertificateFile), "agent-ca.pem")
	if err = os.WriteFile(caPath, ca.CertificatePEM(), 0600); err != nil {
		t.Fatal(err)
	}
	f.config.ClientCAFile = caPath
	service := startServiceFixture(t, f)
	binding := f.config.Repositories[0].Binding
	stateDirectory := t.TempDir()
	state, err := agent.OpenState(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = state.Close() }()
	private, err := agent.LoadOrCreatePrivateKey(state)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	csr, err := agent.CreateCSR(private)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueAgentCertificate(csr, binding.AgentID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	identity := agent.Identity{AgentID: binding.AgentID, HostID: binding.HostID, CertificatePEM: string(issued.CertificatePEM), CABundlePEM: string(ca.CertificatePEM()), NotAfter: issued.NotAfter, ServerName: "control.invalid", GRPCEndpoint: "control.invalid:443"}
	if err = agent.SaveIdentity(state, identity); err != nil {
		t.Fatal(err)
	}
	origin := "https://" + service.listener.Addr().String()
	credential := &agentv1.CredentialRevision{DeliveryId: binding.DeliveryID.String(), Revision: 1, AgentId: binding.AgentID.String(), HostId: binding.HostID.String(), RepositoryId: binding.RepositoryID.String(), GatewayId: binding.GatewayID.String(), GatewayRevision: 1, ResticRevision: 1, Endpoint: origin + "/restic/" + binding.GatewayID.String() + "/" + binding.RepositoryID.String() + "/", CaBundlePem: f.certificate, ResticPassword: []byte(password), ValidFrom: timestamppb.Now()}
	if _, err = state.ApplyRepositoryCredential(identity, credential); err != nil {
		t.Fatal(err)
	}
	// Restic stores ancestor metadata too. Keep the source outside /tmp,
	// where process inputs and parallel fixtures change directory timestamps.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.MkdirTemp(home, ".restfleet-agent-source-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(source) })
	filename := "file ; $(literal).txt"
	contents := []byte("offline scheduled backup content\n")
	if err = os.WriteFile(filepath.Join(source, filename), contents, 0600); err != nil {
		t.Fatal(err)
	}
	plan := domain.PlanSchedule{PlanID: uuid.Must(uuid.NewV7()), Revision: 1, Name: "offline", HostID: binding.HostID, RepositoryID: binding.RepositoryID, Cron: "0 0 * * *", Timezone: "UTC", MisfireGraceSeconds: 120, ConcurrencyPolicy: "FORBID", Paths: []string{source}, ConfigHash: strings.Repeat("b", 64), Execution: &domain.BackupExecution{ResourceLimits: &domain.BackupResourceLimits{LimitUploadKiB: 128}}}
	desired, err := domain.NewDefaultDesiredState(binding.AgentID, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	desired.Plans = []domain.PlanSchedule{plan}
	canonical, err := desired.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	execution, err := json.Marshal(plan.Execution)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &agentv1.DesiredStateSnapshot{Revision: 1, GeneratedAt: timestamppb.New(desired.GeneratedAt), ConfigHash: "sha256:" + hex.EncodeToString(sum[:]), RuntimePolicy: &agentv1.RuntimePolicy{MaxParallelIoJobs: 1, LogLimitBytes: domain.DefaultLogLimitBytes}, Plans: []*agentv1.PlanSchedule{{PlanId: plan.PlanID.String(), Revision: 1, Name: plan.Name, HostId: plan.HostID.String(), RepositoryId: plan.RepositoryID.String(), Cron: plan.Cron, Timezone: plan.Timezone, MisfireGraceSeconds: 120, ConcurrencyPolicy: plan.ConcurrencyPolicy, Paths: plan.Paths, ConfigHash: plan.ConfigHash, ExecutionConfigJson: execution}}}
	if accepted, err := state.ApplyDesiredState(binding.AgentID, snapshot); err != nil || !accepted.Accepted {
		t.Fatal("offline configuration not accepted")
	}
	// Deterministic schedule IDs have second precision; exercise distinct slots.
	nextScheduled := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Second)
	runScheduled := func(expected string, cancelWhileRunning bool) agent.OperationRecord {
		t.Helper()
		scheduled := nextScheduled
		nextScheduled = nextScheduled.Add(time.Second)
		runtime := agent.PlanRuntime{PlanID: plan.PlanID, PlanRevision: 1, ConfigHash: plan.ConfigHash, NextRunAt: scheduled}
		if err := state.PutPlanRuntime(runtime); err != nil {
			t.Fatal(err)
		}
		work, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan error, 1)
		go func() {
			done <- state.RunLocalScheduler(work, 10*time.Millisecond, agent.BackupExecutor{State: state, Binary: resticBinary}, nil)
		}()
		defer func() {
			stop()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error("scheduler failed")
			}
		}()
		operationID := schedule.OperationID(plan.PlanID, scheduled)
		canceled := false
		for {
			record, found, err := state.Operation(operationID)
			if err != nil {
				t.Fatal(err)
			}
			if found && record.Terminal() {
				if record.Status != expected || (expected != agent.OperationCanceled && record.Summary == nil) {
					t.Fatalf("offline backup = %s/%s/%d", record.Status, record.ErrorCode, record.ExitCode)
				}
				if cancelWhileRunning && !canceled {
					t.Fatal("backup completed before cancellation exercised")
				}
				return record
			}
			if cancelWhileRunning && found && record.BytesDone > 0 && !canceled {
				canceled = state.CancelOperation(operationID)
			}
			select {
			case <-ctx.Done():
				t.Fatal("offline backup timed out")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	first := runScheduled(agent.OperationSucceeded, false)
	if len(first.SnapshotID) != 64 || first.Summary.TotalFilesProcessed != 1 {
		t.Fatal("real backup summary missing")
	}
	second := runScheduled(agent.OperationSucceeded, false)
	if second.SnapshotID != "" || second.Summary.TotalFilesProcessed != 1 {
		t.Fatalf("unchanged backup: snapshot=%s, processed=%d, new=%d, changed=%d, unchanged=%d", second.SnapshotID, second.Summary.TotalFilesProcessed, second.Summary.FilesNew, second.Summary.FilesChanged, second.Summary.FilesUnmodified)
	}
	unreadable := filepath.Join(source, "unreadable.txt")
	if err = os.WriteFile(unreadable, []byte("partial read fixture"), 0000); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "readable.txt"), []byte("changed tree despite the unreadable source"), 0600); err != nil {
		t.Fatal(err)
	}
	partial := runScheduled(agent.OperationSucceededWarnings, false)
	if partial.ExitCode != 3 || len(partial.SnapshotID) != 64 || partial.Summary.WarningCount < 1 {
		t.Fatal("real Restic exit 3 lost its warning result")
	}
	if err = os.Chmod(unreadable, 0600); err != nil {
		t.Fatal(err)
	}
	large := make([]byte, 2<<20)
	if _, err = rand.Read(large); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "cancel.bin"), large, 0600); err != nil {
		t.Fatal(err)
	}
	clear(large)
	canceled := runScheduled(agent.OperationCanceled, true)
	if canceled.SnapshotID != "" || !service.repositories[0].owner.ready() {
		t.Fatal("canceled backup left a snapshot or poisoned the owner")
	}
	locks, err := os.ReadDir(filepath.Join(repository, "locks"))
	if err != nil || len(locks) != 0 {
		t.Fatal("Agent failed to clean session-owned lock")
	}
	// Restart preserves both unacknowledged results and the accepted schedule.
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = agent.OpenState(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	reports, err := state.ReportableOperations(time.Now().UTC(), 0)
	if err != nil || len(reports) != 4 {
		t.Fatal("offline results lost across Agent restart")
	}
	for _, record := range reports {
		if record.Uploaded {
			t.Fatal("unacknowledged offline result was discarded")
		}
	}
	if revision, err := state.AcceptedRevision(); err != nil || revision != 1 {
		t.Fatal("last accepted configuration lost")
	}
	root, err := os.MkdirTemp("/dev/shm", "rf-central-read-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	centralRuntime, err := rclone.NewRuntime(root, rcloneBinary)
	if err != nil {
		t.Fatal(err)
	}
	defer centralRuntime.Close()
	reader, err := centralrestic.NewProvisioner(resticBinary, centralRuntime)
	if err != nil {
		t.Fatal(err)
	}
	readBinding := domain.GatewayReadBinding{RepositoryID: binding.RepositoryID, OperationID: uuid.Must(uuid.NewV7()), AdmissionID: binding.AdmissionID, RuntimeID: binding.RuntimeID, ConfigurationHash: binding.ConfigurationHash}
	cert, readKey, err := ca.IssueGatewayReader(readBinding, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(readKey)
	access := &centralrestic.GatewayReadAccess{Origin: origin, Binding: readBinding, CertificatePEM: cert, PrivateKeyPEM: readKey, CABundlePEM: f.certificate}
	indexed, err := reader.SnapshotIndex(ctx, centralrestic.SnapshotIndexRequest{Gateway: access, GatewayID: binding.GatewayID, RepositoryID: binding.RepositoryID, Password: []byte(password)}, nil)
	if err != nil || len(indexed.Entries) != 2 {
		t.Fatal("central reader did not index actual Agent snapshots")
	}
	firstTree := ""
	for _, entry := range indexed.Entries {
		if entry.SnapshotID == first.SnapshotID {
			firstTree = entry.TreeID
		}
	}
	if firstTree == "" {
		t.Fatal("original Agent snapshot missing from index")
	}
	// A new task gets a new reader identity and reads through the same owner.
	readBinding.OperationID = uuid.Must(uuid.NewV7())
	cert, listKey, err := ca.IssueGatewayReader(readBinding, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(listKey)
	access = &centralrestic.GatewayReadAccess{Origin: origin, Binding: readBinding, CertificatePEM: cert, PrivateKeyPEM: listKey, CABundlePEM: f.certificate}
	listed, err := reader.List(ctx, centralrestic.ListRequest{Gateway: access, GatewayID: binding.GatewayID, RepositoryID: binding.RepositoryID, Password: []byte(password), SnapshotID: first.SnapshotID, Path: source}, nil)
	if err != nil || listed.TreeID != firstTree || len(listed.Entries) != 1 || listed.Entries[0].Name != filename || listed.Entries[0].Size != int64(len(contents)) {
		t.Fatal("central list did not match actual backup")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".lock" {
		t.Fatal("central reader materialized a second cloud runtime")
	}
	// Agent storage contains no cloud config, Crypt or central reader private key.
	saved, err := os.ReadFile(filepath.Join(stateDirectory, "repository-credential.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("rclone_config"), []byte("test-refresh"), readKey, listKey} {
		if bytes.Contains(saved, forbidden) {
			t.Fatal("central secret crossed Agent boundary")
		}
	}
	// Restore only the control transport. Lost result/log ACKs must replay the
	// persisted payload and must never execute another backup.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := os.ReadFile(f.config.TLSKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(serverKey)
	serverPair, err := tls.X509KeyPair(f.certificate, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	feedback := &backupFeedbackServer{t: t, agentID: binding.AgentID, results: make(map[string]*agentv1.JobResult), logs: make(map[string]*agentv1.JobLogChunk), finished: make(map[string]bool), done: make(chan struct{})}
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca.CertPool()})))
	agentv1.RegisterAgentControlServiceServer(grpcServer, feedback)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	identity.GRPCEndpoint, identity.ServerName, identity.CABundlePEM = listener.Addr().String(), "127.0.0.1", string(f.certificate)
	if err = agent.SaveIdentity(state, identity); err != nil {
		t.Fatal(err)
	}
	reconnect, stopReconnect := context.WithCancel(ctx)
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.Run(reconnect, state, agent.RunConfig{Version: "test", ResticBinary: resticBinary})
	}()
	defer func() {
		stopReconnect()
		if err := <-agentDone; err != nil {
			t.Error("Agent control loop failed")
		}
	}()
	select {
	case <-feedback.done:
	case <-ctx.Done():
		t.Fatal("results/logs did not replay after control outage")
	}
	if reports, err = state.ReportableOperations(time.Now().UTC(), 0); err != nil || len(reports) != 0 {
		t.Fatal("durable result ACK was not applied")
	}
	entries, err = os.ReadDir(filepath.Join(repository, "snapshots"))
	if err != nil || len(entries) != 2 {
		t.Fatal("control reconnect duplicated a backup")
	}
}

// This peer supplies transport failures only. Production persistence and
// authorization gates are covered separately by PostgreSQL integration tests.
type backupFeedbackServer struct {
	agentv1.UnimplementedAgentControlServiceServer
	t                                                      *testing.T
	agentID                                                uuid.UUID
	mu                                                     sync.Mutex
	results                                                map[string]*agentv1.JobResult
	logs                                                   map[string]*agentv1.JobLogChunk
	finished                                               map[string]bool
	droppedResult, droppedLog, replayedResult, replayedLog bool
	done                                                   chan struct{}
	once                                                   sync.Once
}

func (f *backupFeedbackServer) Connect(stream agentv1.AgentControlService_ConnectServer) error {
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing peer")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return status.Error(codes.Unauthenticated, "missing mTLS")
	}
	id, err := security.AgentIDFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil || id != f.agentID {
		return status.Error(codes.PermissionDenied, "wrong Agent")
	}
	hello, err := stream.Recv()
	if err != nil {
		return err
	}
	if hello.GetHello() == nil {
		return status.Error(codes.InvalidArgument, "missing Hello")
	}
	sequence := uint64(0)
	send := func(message *agentv1.ServerToAgent) error {
		sequence++
		message.Sequence = sequence
		message.MessageId = uuid.Must(uuid.NewV7()).String()
		message.ProtocolVersion = "1.0"
		message.SentAt = timestamppb.Now()
		return stream.Send(message)
	}
	if err = send(&agentv1.ServerToAgent{Payload: &agentv1.ServerToAgent_Welcome{Welcome: &agentv1.Welcome{ConnectionId: uuid.Must(uuid.NewV7()).String(), SelectedProtocolVersion: "1.0", ServerTime: timestamppb.Now(), HeartbeatIntervalSeconds: 5, DesiredConfigRevision: 1, ServerCapabilities: []string{domain.JobResultAckCapability, domain.BackupLogsCapability}}}}); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if result := message.GetJobResult(); result != nil {
			f.mu.Lock()
			if original := f.results[result.OperationId]; original != nil {
				if !proto.Equal(original, result) {
					f.t.Error("lost result ACK changed replayed payload")
				}
				f.replayedResult = true
			} else {
				f.results[result.OperationId] = proto.Clone(result).(*agentv1.JobResult)
			}
			drop := !f.droppedResult
			f.droppedResult = true
			f.mu.Unlock()
			if drop {
				return status.Error(codes.Unavailable, "lost result acknowledgement")
			}
			if err = send(&agentv1.ServerToAgent{Payload: &agentv1.ServerToAgent_JobResultAccepted{JobResultAccepted: &agentv1.JobResultAccepted{OperationId: result.OperationId}}}); err != nil {
				return err
			}
		}
		if chunk := message.GetJobLogChunk(); chunk != nil {
			f.mu.Lock()
			if original := f.logs[chunk.OperationId]; original != nil {
				if !proto.Equal(original, chunk) {
					f.t.Error("lost log ACK changed replayed entries")
				}
				f.replayedLog = true
			} else {
				f.logs[chunk.OperationId] = proto.Clone(chunk).(*agentv1.JobLogChunk)
			}
			var events []domain.BackupLogEntry
			if json.Unmarshal(chunk.EntriesJson, &events) != nil {
				f.t.Error("malformed backup logs")
			}
			for _, event := range events {
				if event.Code == "BACKUP_SUCCEEDED" || event.Code == "BACKUP_SUCCEEDED_WITH_WARNINGS" || event.Code == "CANCELED" {
					f.finished[chunk.OperationId] = true
				}
			}
			drop := !f.droppedLog
			f.droppedLog = true
			complete := !drop && f.replayedResult && f.replayedLog && len(f.results) == 4 && len(f.finished) == 4
			f.mu.Unlock()
			if drop {
				return status.Error(codes.Unavailable, "lost log acknowledgement")
			}
			if err = send(&agentv1.ServerToAgent{Payload: &agentv1.ServerToAgent_JobLogAccepted{JobLogAccepted: &agentv1.JobLogAccepted{OperationId: chunk.OperationId, ThroughSequence: chunk.LastSequence}}}); err != nil {
				return err
			}
			if complete {
				f.once.Do(func() { close(f.done) })
			}
		}
	}
}
