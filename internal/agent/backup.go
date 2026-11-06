package agent

import (
	"context"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/sagehou/restfleet/internal/agent/restic"
	"github.com/sagehou/restfleet/internal/domain"
)

// BackupExecutor runs one scheduled plan through the Gateway REST endpoint.
// It never talks to the Control API: the Gateway session is authorised from the
// local identity and accepted credential.
type BackupExecutor struct {
	State     *State
	Binary    string
	KillGrace time.Duration
}

// Execute satisfies PlanExecutor.
func (e BackupExecutor) Execute(ctx context.Context, plan domain.PlanSchedule, operation uuid.UUID, scheduled, deadline time.Time) (outcome OperationOutcome) {
	if e.State == nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "EXECUTOR_UNAVAILABLE"}
	}
	if plan.Execution!=nil && plan.Execution.ResourceLimits!=nil && plan.Execution.ResourceLimits.MaxDurationSeconds>0 {
		if limited:=time.Now().Add(time.Duration(plan.Execution.ResourceLimits.MaxDurationSeconds)*time.Second);limited.Before(deadline) {deadline=limited}
	}
	ctx,cancel:=context.WithDeadline(ctx,deadline)
	defer cancel()
	if plan.Execution!=nil {
		if err:=e.State.runHooks(ctx,plan.Execution.PreHookIDs);err!=nil {return OperationOutcome{Status:OperationFailed,ErrorCode:"PRE_HOOK_FAILED"}}
		defer func(){
			if ctx.Err()==nil {
				if err:=e.State.runHooks(ctx,plan.Execution.PostHookIDs);err!=nil && outcome.Status==OperationSucceeded {
					outcome.Status,outcome.ErrorCode=OperationSucceededWarnings,"POST_HOOK_FAILED"
				}
			}
		}()
	}
	identity, err := LoadIdentity(e.State)
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "IDENTITY_UNAVAILABLE"}
	}
	credential, found, err := e.State.LoadRepositoryCredential(identity)
	if err != nil || !found {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "CREDENTIAL_UNAVAILABLE"}
	}
	defer credential.Clear()
	directory, err := os.MkdirTemp("", "restfleet-run-")
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "WORKDIR_UNAVAILABLE"}
	}
	defer func() { _ = os.RemoveAll(directory) }()
	if err := os.Chmod(directory, 0o700); err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "WORKDIR_UNAVAILABLE"}
	}
	passwordFile, err := writeSecretFile(directory, "restic-password", credential.ResticPassword)
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "WORKDIR_UNAVAILABLE"}
	}
	caFile, err := writeSecretFile(directory, "gateway-ca.pem", credential.CABundlePEM)
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "WORKDIR_UNAVAILABLE"}
	}
	certFile, err := e.writeClientCertificate(directory)
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "IDENTITY_UNAVAILABLE"}
	}
	prepared, err := restic.Prepare(plan.Paths, plan.ExcludePatterns)
	if err != nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "CONFIG_INVALID"}
	}
	defer func() { _ = prepared.Cleanup() }()
	request := restic.BackupRequest{
		PasswordFile: passwordFile, HostID: plan.HostID, PlanID: plan.PlanID,
		Paths: plan.Paths, ExcludePatterns: plan.ExcludePatterns,
		ExcludeCaches: plan.ExcludeCaches, OneFileSystem: plan.OneFileSystem,
		TLSClientCertFile: certFile, CACertFile: caFile, SkipIfUnchanged: true,
	}
	request.CacheDirectory=filepath.Join(e.State.Directory(),"cache")
	if os.MkdirAll(request.CacheDirectory,0700)!=nil {return OperationOutcome{Status:OperationFailed,ErrorCode:"WORKDIR_UNAVAILABLE"}}
	if plan.Execution!=nil && plan.Execution.ResourceLimits!=nil {
		limits:=plan.Execution.ResourceLimits
		request.LimitUploadKiB,request.LimitDownloadKiB,request.ReadConcurrency=limits.LimitUploadKiB,limits.LimitDownloadKiB,limits.ReadConcurrency
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	progress := &progressSink{state: e.State, operation: operation, cancel: stop}
	var runResult restic.Result
	var runErr error
	sessionErr := e.State.WithGatewaySession(runCtx, operation, deadline, func(sessionCtx context.Context, endpoint string) error {
		request.Repository = endpoint
		args, err := restic.BackupArgs(request, prepared)
		if err != nil {
			return err
		}
		runResult, runErr = restic.Run{
			Binary: e.Binary, Args: args, Env: restic.Env(request),
			Stdout: progress, KillGrace: e.KillGrace,
		}.Execute(sessionCtx)
		return runErr
	})
	progress.finish()
	if errors.Is(sessionErr,ErrGatewayCleanup) {return OperationOutcome{Status:OperationFailed,ErrorCode:"GATEWAY_CLEANUP_UNCONFIRMED",ExitCode:runResult.ExitCode}}
	if errors.Is(sessionErr,ErrGatewayBusy) {return OperationOutcome{Status:OperationFailed,ErrorCode:"GATEWAY_BUSY",ExitCode:runResult.ExitCode}}
	if progress.errorCode != "" {
		return OperationOutcome{Status: OperationFailed, ErrorCode: progress.errorCode, ExitCode: runResult.ExitCode}
	}
	if errors.Is(ctx.Err(),context.DeadlineExceeded) {return OperationOutcome{Status:OperationFailed,ErrorCode:"BACKUP_TIMED_OUT",ExitCode:runResult.ExitCode}}
	if errors.Is(runErr, restic.ErrCanceled) || errors.Is(sessionErr, context.Canceled) {
		return OperationOutcome{Status: OperationCanceled, ErrorCode: "CANCELED", ExitCode: runResult.ExitCode,
			FilesDone: progress.files, TotalFiles: progress.totalFiles, BytesDone: progress.bytes,
			TotalBytes: progress.totalBytes, FilesNew: progress.filesNew, DataAdded: progress.dataAdded}
	}
	if sessionErr != nil && runErr == nil {
		return OperationOutcome{Status: OperationFailed, ErrorCode: "GATEWAY_SESSION_FAILED"}
	}
	return classify(runResult.ExitCode, progress.snapshotID, progress.sawSummary, progress)
}

func classify(exitCode int, snapshotID string, sawSummary bool, progress *progressSink) OperationOutcome {
	outcome := OperationOutcome{ExitCode: exitCode, SnapshotID: snapshotID,
		FilesDone: progress.files, TotalFiles: progress.totalFiles,
		BytesDone: progress.bytes, TotalBytes: progress.totalBytes,
		FilesNew: progress.filesNew, DataAdded: progress.dataAdded}
	outcome.Summary=progress.summary
	switch exitCode {
	case 0:
		if !sawSummary {
			outcome.Status, outcome.ErrorCode = OperationFailed, "INVALID_ENGINE_OUTPUT"
			return outcome
		}
		outcome.Status = OperationSucceeded
	case 3:
		if !sawSummary || snapshotID == "" {
			outcome.Status, outcome.ErrorCode = OperationFailed, "INVALID_ENGINE_OUTPUT"
			return outcome
		}
		outcome.Status = OperationSucceededWarnings
		outcome.ErrorCode = "PARTIAL_READ"
	case 130:
		outcome.Status, outcome.ErrorCode = OperationCanceled, "CANCELED"
	case 11:
		outcome.Status, outcome.ErrorCode = OperationFailed, "REPOSITORY_LOCKED"
	case 12:
		outcome.Status, outcome.ErrorCode = OperationFailed, "WRONG_REPOSITORY_PASSWORD"
	case 10:
		outcome.Status, outcome.ErrorCode = OperationFailed, "REPOSITORY_MISSING"
	case 1:
		outcome.Status, outcome.ErrorCode = OperationFailed, "BACKUP_FAILED"
	default:
		outcome.Status, outcome.ErrorCode = OperationFailed, "UNKNOWN_EXIT_CODE"
	}
	return outcome
}

func writeSecretFile(directory, name string, payload []byte) (string, error) {
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return "", err
	}
	return path, file.Close()
}

// writeClientCertificate combines the Agent certificate and private key, which
// is the form Restic expects for --tls-client-cert.
func (e BackupExecutor) writeClientCertificate(directory string) (string, error) {
	identity, err := LoadIdentity(e.State)
	if err != nil {
		return "", err
	}
	privateKey, err := os.ReadFile(filepath.Join(e.State.Directory(), privateKeyFile))
	if err != nil {
		return "", err
	}
	defer clear(privateKey)
	combined := make([]byte, 0, len(identity.CertificatePEM)+len(privateKey))
	combined = append(combined, identity.CertificatePEM...)
	combined = append(combined, privateKey...)
	defer clear(combined)
	return writeSecretFile(directory, "client.pem", combined)
}

// progressSink parses Restic JSONL and persists throttled progress so a crash
// loses at most a couple of seconds of counters.
type progressSink struct {
	state       *State
	operation   uuid.UUID
	sawSummary  bool
	snapshotID  string
	files       int64
	totalFiles  int64
	bytes       int64
	totalBytes  int64
	filesNew    int64
	dataAdded   int64
	lastWrite   time.Time
	lastPercent float64
	pendingLine []byte
	summary *domain.BackupSummary
	warnings int64
	errorCode string
	cancel context.CancelFunc
}

func (p *progressSink) Write(chunk []byte) (int, error) {
	length := len(chunk)
	for len(chunk) > 0 && p.errorCode == "" {
		index := bytes.IndexByte(chunk, '\n')
		part := chunk
		if index >= 0 {
			part = chunk[:index]
		}
		if len(p.pendingLine)+len(part) > 1<<20 {
			p.fail("INVALID_ENGINE_OUTPUT")
			break
		}
		p.pendingLine = append(p.pendingLine, part...)
		if index < 0 {
			break
		}
		p.consume(p.pendingLine)
		p.pendingLine = p.pendingLine[:0]
		chunk = chunk[index+1:]
	}
	return length, nil
}

func (p *progressSink) finish() {
	if p.errorCode == "" && len(p.pendingLine) > 0 {
		p.consume(p.pendingLine)
	}
	p.pendingLine = nil
}

func (p *progressSink) fail(code string) {
	if p.errorCode == "" {
		p.errorCode = code
		p.pendingLine = nil
		if p.cancel != nil {
			p.cancel()
		}
	}
}

func (p *progressSink) consume(line []byte) {
	event, ok, err := restic.ParseEvent(line)
	if err != nil {
		p.fail("INVALID_ENGINE_OUTPUT")
		return
	}
	if !ok {
		return
	}
	switch event.Type {
	case "status":
		p.files, p.totalFiles = event.FilesDone, event.TotalFiles
		p.bytes, p.totalBytes = event.BytesDone, event.TotalBytes
		now := time.Now()
		if now.Sub(p.lastWrite) >= 2*time.Second || event.PercentDone-p.lastPercent >= 0.05 {
			p.lastWrite, p.lastPercent = now, event.PercentDone
			p.persist()
		}
	case "summary":
		if p.sawSummary {
			p.fail("INVALID_ENGINE_OUTPUT")
			return
		}
		p.sawSummary = true
		p.snapshotID = event.SnapshotID
		p.summary=event.Summary
		if p.summary!=nil {p.summary.WarningCount=p.warnings}
		p.filesNew, p.dataAdded = event.FilesNew, event.DataAdded
		if event.FilesDone > 0 {
			p.files = event.FilesDone
		}
		p.bytes=event.BytesDone
		p.persist()
	case "error":
		p.warnings++
	}
}

func (p *progressSink) persist() {
	if p.state == nil {
		return
	}
	if err := p.state.updateOperation(p.operation, func(record *OperationRecord) {
		if record.Terminal() {
			return
		}
		record.FilesDone, record.TotalFiles = p.files, p.totalFiles
		record.BytesDone, record.TotalBytes = p.bytes, p.totalBytes
		record.FilesNew, record.DataAdded = p.filesNew, p.dataAdded
		record.Summary=p.summary
		if p.snapshotID != "" {
			record.SnapshotID = p.snapshotID
		}
	}); err != nil {
		p.fail("STATE_STORAGE_UNAVAILABLE")
	}
}
