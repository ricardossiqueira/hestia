package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
)

// handleV2PublishCommand is plain JSON over HTTP, not Connect-RPC, matching
// internal/apigateway/device_v2.go's style - it is the only caller, reaching
// this over the loopback internal_address (see this package's doc comment
// and ADR-009). The caller has already resolved the v2 device, validated
// the command against its manifest and built the final envelope; this only
// publishes it, via CommandPublisher.PublishRaw.
func (s *Server) handleV2PublishCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		Topic   string          `json:"topic"`
		Payload json.RawMessage `json:"payload"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.Topic) == "" || len(request.Payload) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := s.publisher.PublishRaw(r.Context(), request.Topic, request.Payload); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetQueueSummary mirrors outbox.Snapshot 1:1 - the outbox's current
// pending state, not the lifetime counters GetStatus already reports.
func (s *Server) GetQueueSummary(ctx context.Context, req *connect.Request[apiv1.GetQueueSummaryRequest]) (*connect.Response[apiv1.GetQueueSummaryResponse], error) {
	snap, err := s.queue.Snapshot(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read queue snapshot: %w", err))
	}
	var oldest *timestamppb.Timestamp
	if snap.OldestEnqueuedAt != nil {
		oldest = timestamppb.New(*snap.OldestEnqueuedAt)
	}
	return connect.NewResponse(&apiv1.GetQueueSummaryResponse{
		PendingMessages:  uint64(snap.Messages),
		PendingBytes:     snap.PayloadBytes,
		OldestEnqueuedAt: oldest,
	}), nil
}

// GetRecentEvents mirrors mqtt.Gateway.RecentEvents 1:1 - already most
// recent first, already payload-free, already capped - nothing to filter
// or reorder here.
func (s *Server) GetRecentEvents(ctx context.Context, req *connect.Request[apiv1.GetRecentEventsRequest]) (*connect.Response[apiv1.GetRecentEventsResponse], error) {
	filter := mqtt.EventFilter{
		DeviceID:       req.Msg.GetDeviceId(),
		Limit:          int(req.Msg.GetLimit()),
		BeforeSequence: req.Msg.GetBeforeSequence(),
	}
	if req.Msg.GetSince() != nil {
		filter.Since = req.Msg.GetSince().AsTime()
	}
	source, hasMore := s.events.RecentEvents(filter)
	events := make([]*apiv1.ActivityEvent, 0, len(source))
	for _, event := range source {
		events = append(events, &apiv1.ActivityEvent{
			Sequence:  event.Sequence,
			Timestamp: timestamppb.New(event.Timestamp),
			DeviceId:  event.DeviceID,
			Kind:      string(event.Kind),
			Topic:     event.Topic,
			Outcome:   event.Outcome,
			Detail:    event.Detail,
		})
	}
	return connect.NewResponse(&apiv1.GetRecentEventsResponse{Events: events, HasMore: hasMore}), nil
}

// GetStatus mirrors mqtt.Snapshot 1:1 - see api.proto's GetStatusResponse
// doc comment.
func (s *Server) GetStatus(ctx context.Context, req *connect.Request[apiv1.GetStatusRequest]) (*connect.Response[apiv1.GetStatusResponse], error) {
	snap := s.status.Snapshot()
	var startedAt *timestamppb.Timestamp
	if snap.StartedAt != nil {
		startedAt = timestamppb.New(*snap.StartedAt)
	}
	return connect.NewResponse(&apiv1.GetStatusResponse{
		StartedAt:            startedAt,
		Started:              snap.Started,
		MqttConnected:        snap.MQTTConnected,
		Subscriptions:        int32(snap.Subscriptions),
		AcceptedMessages:     snap.AcceptedMessages,
		RejectedMessages:     snap.RejectedMessages,
		LocalRoutesPublished: snap.LocalRoutesPublished,
		LocalRoutesFailed:    snap.LocalRoutesFailed,
		OutboxStored:         snap.OutboxStored,
		OutboxDiscarded:      snap.OutboxDiscarded,
		OutboxFailed:         snap.OutboxFailed,
	}), nil
}
