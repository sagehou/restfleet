package agent

import (
	"time"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reportOperations uploads pending progress and terminal results. Delivery is
// at-least-once; the terminal record is only marked uploaded after the stream
// accepted the message, so a disconnect simply replays it.
func reportOperations(stream agentv1.AgentControlService_ConnectClient, sequence *uint64,
	protocol string, state *State, progressInterval time.Duration,
) error {
	records, err := state.ReportableOperations(time.Now().UTC(), progressInterval)
	if err != nil {
		return err
	}
	for _, record := range records {
		scheduled := timestamppb.New(record.ScheduledAt)
		if record.Terminal() {
			payload := &agentv1.AgentToServer{Payload: &agentv1.AgentToServer_JobResult{
				JobResult: &agentv1.JobResult{
					OperationId: record.ID.String(), PlanId: record.PlanID.String(),
					PlanRevision: record.PlanRevision, ScheduledAt: scheduled,
					Status: record.Status, ErrorCode: record.ErrorCode, ExitCode: int64(record.ExitCode),
					SnapshotId: record.SnapshotID, FilesNew: record.FilesDone,
					DataAdded: record.BytesDone, FinishedAt: timestamppb.New(record.FinishedAt),
				},
			}}
			if err := sendAgentMessage(stream, sequence, protocol, payload); err != nil {
				return err
			}
			record.Uploaded = true
			if err := state.PutOperation(record); err != nil {
				return err
			}
			continue
		}
		payload := &agentv1.AgentToServer{Payload: &agentv1.AgentToServer_JobProgress{
			JobProgress: &agentv1.JobProgress{
				OperationId: record.ID.String(), PlanId: record.PlanID.String(),
				PlanRevision: record.PlanRevision, ScheduledAt: scheduled,
				Sequence: uint64(time.Now().UnixMilli()), Stage: "backup",
				FilesDone: record.FilesDone, TotalFiles: record.TotalFiles,
				BytesDone: record.BytesDone, TotalBytes: record.TotalBytes,
				ObservedAt: timestamppb.Now(),
			},
		}}
		if err := sendAgentMessage(stream, sequence, protocol, payload); err != nil {
			return err
		}
		record.LastReportedAt = time.Now().UTC()
		if err := state.PutOperation(record); err != nil {
			return err
		}
	}
	return nil
}
