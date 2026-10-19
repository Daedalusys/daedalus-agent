// ReportStatus handler.
//
// Wire-protocol role:
//
//   - 审计事件回报;agent 完成本地事务后批量推
//
// Behaviour (T7 replaces T5 stub):
//
//  1. Validate req is non-nil.
//  2. Read events from the per-process audit chain whose
//     seq > since_audit_seq, capped at max_events.
//  3. Convert each audit record to the wire AuditEvent
//     proto (Seq, TimestampUnix, EventType, PayloadJson,
//     PrevHash, CurrentHash).
//  4. Return events + final_seq = the highest seq we
//     returned (= the chain's current tip).
//
// Edge cases:
//   - audit is nil (test / development mode): return empty
//     events + final_seq=0. Matches the T5 stub's wire
//     shape so clients don't see a regression.
//   - since_audit_seq >= current tip: return empty events
//   - final_seq = current tip. (The client is caught up.)
//   - max_events == 0: no cap. T7 returns everything > since.
//
// Concurrency: the audit chain is safe for concurrent reads
// (audit.Local.Since opens the file read-only); we serve
// multiple ReportStatus calls in parallel.
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/Daedalusys/daedalus-sdk/proto/agent/v1"
)

// maxReportEvents is the hard upper bound on the number of
// events returned in a single ReportStatus response, even if
// the caller passed a higher max_events. The control plane
// gRPC message has a 4MB default ceiling; each event can be
// 200-500 bytes after JSON serialisation, so 10k is a safe
// per-response cap.
const maxReportEvents = 10000

// ReportStatus implements agentv1.AgentServiceServer.ReportStatus.
//
// Validation:
//   - req must be non-nil (InvalidArgument otherwise).
//
// Response (T7):
//   - events: list of AuditEvent protos whose seq > since_audit_seq,
//     capped at min(max_events, maxReportEvents).
//   - final_seq: the highest seq currently in the chain
//     (i.e. the tip the client should pass as its next
//     since_audit_seq).
func (s *agentService) ReportStatus(ctx context.Context, req *agentv1.ReportStatusRequest) (*agentv1.ReportStatusResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "ReportStatus: nil request")
	}

	// No audit chain configured (development / unit test
	// without audit). Return the documented "no audit yet"
	// wire shape: empty events, final_seq=0.
	if s.audit == nil {
		return &agentv1.ReportStatusResponse{
			Events:   []*agentv1.AuditEvent{},
			FinalSeq: 0,
		}, nil
	}

	since := req.GetSinceAuditSeq()
	maxEvents := req.GetMaxEvents()
	if maxEvents == 0 || maxEvents > maxReportEvents {
		maxEvents = maxReportEvents
	}

	records, finalSeq, err := s.audit.Since(since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ReportStatus: read audit chain: %v", err)
	}

	events := make([]*agentv1.AuditEvent, 0, len(records))
	for _, rec := range records {
		if uint32(len(events)) >= maxEvents {
			break
		}
		ts, terr := parseAuditTimestamp(rec.Timestamp)
		if terr != nil {
			// Defensive: a malformed timestamp on disk
			// shouldn't fail the whole report; the
			// control plane will see TimestampUnix=0
			// and can re-Verify the chain offline.
			ts = 0
		}
		// Pack the args + outcome + identity into
		// PayloadJson as a JSON object. The wire proto
		// has no dedicated Outcome / Identity fields;
		// the control plane unpacks the JSON to recover
		// them. EventType carries the SDK tool name
		// (e.g. "apply_intent.success") so the
		// control plane can route without unpacking.
		payload := struct {
			Identity string            `json:"identity"`
			Outcome  string            `json:"outcome"`
			Args     map[string]string `json:"args,omitempty"`
		}{
			Identity: rec.Identity,
			Outcome:  rec.Outcome,
			Args:     rec.Args,
		}
		payloadJSON, perr := json.Marshal(payload)
		if perr != nil {
			// Should never happen for our flat
			// string args; fall back to outcome
			// alone so the report still goes out.
			payloadJSON = []byte(fmt.Sprintf(`{"outcome":%q}`, rec.Outcome))
		}
		events = append(events, &agentv1.AuditEvent{
			Seq:           rec.Seq,
			TimestampUnix: ts,
			EventType:     rec.Tool,
			PayloadJson:   string(payloadJSON),
			PrevHash:      rec.PrevHash,
			CurrentHash:   rec.EntryHash,
		})
	}

	return &agentv1.ReportStatusResponse{
		Events:   events,
		FinalSeq: finalSeq,
	}, nil
}

// parseAuditTimestamp parses the SDK's timestamp format
// (RFC3339 with microsecond precision) and returns the
// Unix-epoch second.
//
// The SDK's FormatTimestamp is documented as
// "YYYY-MM-DDTHH:MM:SS.ffffff+00:00" with the fractional
// part omitted when microseconds == 0. Both forms parse
// cleanly with time.Parse(time.RFC3339Nano, ...).
func parseAuditTimestamp(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty timestamp")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, err
	}
	return t.Unix(), nil
}
