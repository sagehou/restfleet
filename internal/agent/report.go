package agent

import (
	"encoding/json"
	"time"
	"github.com/google/uuid"

	agentv1 "github.com/sagehou/restfleet/api/proto/gen/go/restfleet/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// reportOperations uploads pending progress and terminal results. Delivery is
// at-least-once; capable peers acknowledge durable receipt before results leave
// the pending set. Each batch is bounded so heartbeats and cancels can proceed.
func reportOperations(stream agentv1.AgentControlService_ConnectClient, sequence *uint64,
	protocol string, state *State, progressInterval time.Duration, resultAck bool,
) error {
	records, err := state.ReportableOperations(time.Now().UTC(), progressInterval)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Terminal() {
			if err := sendOperationResult(stream, sequence, protocol, record); err != nil {
				return err
			}
			if !resultAck {
				if err := state.MarkOperationUploaded(record.ID); err != nil {
					return err
				}
			}
			if err := state.markOperationReported(record.ID, time.Now().UTC()); err != nil {
				return err
			}
			continue
		}
		scheduled := timestamppb.New(record.ScheduledAt)
		payload := &agentv1.AgentToServer{Payload: &agentv1.AgentToServer_JobProgress{
			JobProgress: &agentv1.JobProgress{
				OperationId: record.ID.String(), PlanId: record.PlanID.String(),
				PlanRevision: record.PlanRevision, ScheduledAt: scheduled,
				ParentOperationId:optionalOperationID(record.ParentOperationID),RootOperationId:optionalOperationID(record.RootOperationID),Attempt:int32(record.Attempt),
				Sequence: uint64(time.Now().UnixMilli()), Stage: "backup",
				FilesDone: record.FilesDone, TotalFiles: record.TotalFiles,
				BytesDone: record.BytesDone, TotalBytes: record.TotalBytes,
				ObservedAt: timestamppb.Now(),
			},
		}}
		if err := sendAgentMessage(stream, sequence, protocol, payload); err != nil {
			return err
		}
		if err := state.markOperationReported(record.ID, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

// sendOperationResult replays one stored terminal result. It is used for normal
// upload and for a duplicate JobDispatch of an operation that already finished.
func sendOperationResult(
	stream agentv1.AgentControlService_ConnectClient,
	sequence *uint64,
	protocol string,
	record OperationRecord,
) error {
	var summary []byte
	if record.Summary!=nil {
		var err error
		summary,err=json.Marshal(record.Summary);if err!=nil{return err}
	}
	return sendAgentMessage(stream, sequence, protocol, &agentv1.AgentToServer{
		Payload: &agentv1.AgentToServer_JobResult{
			JobResult: &agentv1.JobResult{
				OperationId: record.ID.String(), PlanId: record.PlanID.String(),
				PlanRevision: record.PlanRevision, ScheduledAt: timestamppb.New(record.ScheduledAt),
				ParentOperationId:optionalOperationID(record.ParentOperationID),RootOperationId:optionalOperationID(record.RootOperationID),Attempt:int32(record.Attempt),
				Status: record.Status, ErrorCode: record.ErrorCode, ExitCode: int64(record.ExitCode),
				SnapshotId: record.SnapshotID, FilesNew: record.FilesNew,
				SummaryJson:summary,
				DataAdded: record.DataAdded, FinishedAt: timestamppb.New(record.FinishedAt),
			},
		},
	})
}

func optionalOperationID(id uuid.UUID) string {
	if id==uuid.Nil{return ""}
	return id.String()
}
