package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/internal/devicemanifest"
	"github.com/ricardossiqueira/iot-gateway/internal/deviceprofile"
	"github.com/ricardossiqueira/iot-gateway/internal/mqtt"
)

// ListDevices returns every configured device, sorted by ID. It never
// filters by enabled state - a disabled device is still visible so a
// client understands why publishing to it fails.
func (s *Server) ListDevices(ctx context.Context, req *connect.Request[apiv1.ListDevicesRequest]) (*connect.Response[apiv1.ListDevicesResponse], error) {
	current := s.currentDevices()
	devices := make([]*apiv1.Device, 0, len(current))
	for _, d := range current {
		devices = append(devices, &apiv1.Device{
			Id:      d.ID,
			Type:    d.Type,
			Enabled: d.Enabled != nil && *d.Enabled,
			Profile: d.Profile,
			Topics: &apiv1.DeviceTopics{
				Telemetry:     d.Topics.Telemetry,
				State:         d.Topics.State,
				Event:         d.Topics.Event,
				Command:       d.Topics.Command,
				CommandResult: d.Topics.CommandResult,
			},
		})
	}
	return connect.NewResponse(&apiv1.ListDevicesResponse{Devices: devices}), nil
}

// ListDeviceCommands describes the commands a device accepts. A device
// without a profile returns schema_validated=false and an empty commands
// list - see docs/api-v1.md's fallback-opaco section.
func (s *Server) ListDeviceCommands(ctx context.Context, req *connect.Request[apiv1.ListDeviceCommandsRequest]) (*connect.Response[apiv1.ListDeviceCommandsResponse], error) {
	deviceID := strings.TrimSpace(req.Msg.GetDeviceId())
	device, ok := s.deviceByID(deviceID)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown device %q", deviceID))
	}
	if s.manifestResolver != nil {
		document, found, err := s.manifestResolver.ResolveDeviceManifest(ctx, deviceID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if found {
			commands := make([]*apiv1.CommandDescriptor, 0, len(document.Capabilities.Commands))
			for _, command := range document.Capabilities.Commands {
				commands = append(commands, &apiv1.CommandDescriptor{Type: command.Type, ParametersJson: string(command.Parameters)})
			}
			return connect.NewResponse(&apiv1.ListDeviceCommandsResponse{DeviceId: deviceID, SchemaValidated: true, Commands: commands}), nil
		}
	}
	if device.Profile == "" {
		return connect.NewResponse(&apiv1.ListDeviceCommandsResponse{
			DeviceId:        deviceID,
			SchemaValidated: false,
		}), nil
	}
	descriptors, err := deviceprofile.Describe(device.Profile)
	if err != nil {
		// The registry rejected a profile name that Config.Validate already
		// confirmed exists - only possible if the process's compiled
		// registry and the validated configuration disagree, which is
		// itself a bug (not a client error).
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	commands := make([]*apiv1.CommandDescriptor, 0, len(descriptors))
	for _, d := range descriptors {
		commands = append(commands, &apiv1.CommandDescriptor{
			Type:              d.Type,
			ParametersMessage: d.ParametersMessage,
			ParametersSchema:  d.ParametersSchema,
		})
	}
	return connect.NewResponse(&apiv1.ListDeviceCommandsResponse{
		DeviceId:        deviceID,
		SchemaValidated: true,
		Commands:        commands,
	}), nil
}

// commandEnvelope is the outgoing MQTT payload shape docs/mqtt.md and
// internal/mqtt's validateCommand require: command_id (server-generated
// UUID), type, parameters (a JSON object) - identical contract to the one
// internal/commandapi used to build.
type commandEnvelope struct {
	CommandID  string          `json:"command_id"`
	Type       string          `json:"type"`
	Parameters json.RawMessage `json:"parameters"`
}

// PublishCommand resolves the device, validates parameters against its
// profile when it has one (opaque fallback otherwise), and publishes a
// fire-and-forget command through CommandPublisher. Every rejection this
// method or the publisher can return (unknown device, schema violation,
// disabled device, missing command topic) is something the caller can fix
// by changing the request, so all of them collapse to
// connect.CodeInvalidArgument - the same MVP simplification
// internal/commandapi made with a fixed HTTP 400, now expressed as a gRPC
// code.
func (s *Server) PublishCommand(ctx context.Context, req *connect.Request[apiv1.PublishCommandRequest]) (*connect.Response[apiv1.PublishCommandResponse], error) {
	deviceID := strings.TrimSpace(req.Msg.GetDeviceId())
	commandType := strings.TrimSpace(req.Msg.GetType())
	if deviceID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("device_id is required"))
	}
	if commandType == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("type is required"))
	}
	device, ok := s.deviceByID(deviceID)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown device %q", deviceID))
	}

	parameters := req.Msg.GetParameters()
	if parameters == nil {
		parameters = &structpb.Struct{}
	}
	// Struct -> JSON: numbers become float64 (ADR-012's documented
	// precision limitation). Acceptable because parameters are validated
	// and recodified against a schema immediately below when the device
	// has a profile; devices without a profile inherit the same
	// limitation internal/commandapi already had for any client that
	// JSON-decoded then re-encoded a number.
	paramsJSON, err := protojson.Marshal(parameters)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode parameters: %w", err))
	}

	finalParams := paramsJSON
	schemaValidated := false
	if s.manifestResolver != nil {
		document, found, resolveErr := s.manifestResolver.ResolveDeviceManifest(ctx, deviceID)
		if resolveErr != nil {
			return nil, connect.NewError(connect.CodeInternal, resolveErr)
		}
		if found {
			canonical, validationErr := devicemanifest.ValidateCommandParameters(document, commandType, paramsJSON)
			if validationErr != nil {
				return nil, connect.NewError(connect.CodeInvalidArgument, validationErr)
			}
			finalParams, schemaValidated = canonical, true
		}
	}
	if !schemaValidated && device.Profile != "" {
		canonical, err := deviceprofile.ValidateAndCanonicalize(device.Profile, commandType, paramsJSON)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		finalParams = canonical
		schemaValidated = true
	}

	commandID := uuid.NewString()
	payload, err := json.Marshal(commandEnvelope{CommandID: commandID, Type: commandType, Parameters: finalParams})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode command envelope: %w", err))
	}

	if err := s.publisher.PublishCommand(ctx, deviceID, payload); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	return connect.NewResponse(&apiv1.PublishCommandResponse{
		CommandId:       commandID,
		DeviceId:        deviceID,
		SchemaValidated: schemaValidated,
		PublishedAt:     timestamppb.Now(),
	}), nil
}

// GetDeviceTelemetry returns the most recently accepted Telemetry-kind
// payload for a device, cached only in memory by internal/mqtt.Gateway - no
// history, no persistence. A device that exists but has not published
// telemetry yet (or never declares a telemetry topic) is not an error:
// the response comes back with available=false.
func (s *Server) GetDeviceTelemetry(ctx context.Context, req *connect.Request[apiv1.GetDeviceTelemetryRequest]) (*connect.Response[apiv1.GetDeviceTelemetryResponse], error) {
	deviceID := strings.TrimSpace(req.Msg.GetDeviceId())
	if _, ok := s.deviceByID(deviceID); !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown device %q", deviceID))
	}

	payload, observedAt, ok := s.telemetry.LastTelemetry(deviceID)
	if !ok {
		return connect.NewResponse(&apiv1.GetDeviceTelemetryResponse{DeviceId: deviceID, Available: false}), nil
	}

	var payloadStruct structpb.Struct
	if err := protojson.Unmarshal(payload, &payloadStruct); err != nil {
		// internal/mqtt only caches payloads that already passed
		// validateInbound's JSON-object check, so this can only mean a bug
		// in that guarantee, not a client error.
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("decode cached telemetry payload: %w", err))
	}

	return connect.NewResponse(&apiv1.GetDeviceTelemetryResponse{
		DeviceId:   deviceID,
		Available:  true,
		Payload:    &payloadStruct,
		ObservedAt: timestamppb.New(observedAt),
	}), nil
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
