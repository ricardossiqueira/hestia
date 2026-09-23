package apigateway

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	// Importing internal/admin here (unlike internal/api, deliberately
	// never imported by this package - see deviceToProto's comment) is
	// fine: it is only for the four sentinel error values (public
	// vocabulary, not the concrete *admin.Server type - DeviceAdmin above
	// still fully decouples the actual method calls). Server.go's
	// DeviceAdmin interface is designed specifically so *admin.Server
	// satisfies it and the two are constructed together in the same
	// process by cmd/gateway's runAdmin - unlike internal/api, which is
	// proxied over the network and could in principle run elsewhere.
	"github.com/ricardossiqueira/iot-gateway/internal/admin"
	"github.com/ricardossiqueira/iot-gateway/internal/config"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// deviceToProto is deliberately its own copy of the identical mapping in
// internal/api/handlers.go's ListDevices, not a shared/exported helper:
// internal/api and internal/apigateway communicate only over the network
// (the reverse proxy), never via a direct Go import between them, even
// though they happen to run in the same OS process today - same reasoning
// already applied to duplicating basicAuth/cors instead of extracting a
// shared package for them.
func deviceToProto(d config.Device) *apiv1.Device {
	return &apiv1.Device{
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
	}
}

func routeToProto(route config.Route) *apiv1.Route {
	return &apiv1.Route{
		Id: route.ID, SourceTopic: route.SourceTopic, DestinationTopic: route.DestinationTopic,
		CommandType: route.Transform.CommandType, Qos: uint32(route.QoS), Retain: route.Retain,
	}
}

func routeFromProto(route *apiv1.Route) (config.Route, error) {
	if route == nil {
		return config.Route{}, errors.New("route is required")
	}
	if route.GetQos() > 2 {
		return config.Route{}, errors.New("route.qos must be between 0 and 2")
	}
	return config.Route{
		ID: route.GetId(), SourceTopic: route.GetSourceTopic(), DestinationTopic: route.GetDestinationTopic(),
		Transform: config.RouteTransform{Type: "json_command", CommandType: route.GetCommandType()},
		QoS:       byte(route.GetQos()), Retain: route.GetRetain(),
	}, nil
}

// RegisterExistingDevice adopts a pre-existing broker identity without ever
// returning or rotating its password. This keeps local collectors online while
// making their inbound topics visible to the runtime registry.
func (s *Server) RegisterExistingDevice(ctx context.Context, req *connect.Request[apiv1.RegisterExistingDeviceRequest]) (*connect.Response[apiv1.RegisterExistingDeviceResponse], error) {
	device, err := s.cfg.Admin.RegisterExistingDevice(ctx, req.Msg.GetDeviceId(), req.Msg.GetTemplate())
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrDeviceAlreadyExists):
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		case errors.Is(err, admin.ErrUnknownTemplate), errors.Is(err, admin.ErrInvalidDeviceID), errors.Is(err, admin.ErrTemplateRequiresAdopt):
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&apiv1.RegisterExistingDeviceResponse{Device: deviceToProto(device), AppliedAt: timestamppb.Now()}), nil
}

func (s *Server) ListRoutes(ctx context.Context, _ *connect.Request[apiv1.ListRoutesRequest]) (*connect.Response[apiv1.ListRoutesResponse], error) {
	routes, err := s.cfg.Admin.ListRoutes(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response := &apiv1.ListRoutesResponse{Routes: make([]*apiv1.Route, 0, len(routes))}
	for _, route := range routes {
		response.Routes = append(response.Routes, routeToProto(route))
	}
	return connect.NewResponse(response), nil
}

// CreateRoute changes only the revisioned SQLite routing policy. The running
// gateway converges on it itself; this RPC must never restart a service.
func (s *Server) CreateRoute(ctx context.Context, req *connect.Request[apiv1.CreateRouteRequest]) (*connect.Response[apiv1.CreateRouteResponse], error) {
	route, err := routeFromProto(req.Msg.GetRoute())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := s.cfg.Admin.CreateRoute(ctx, route); err != nil {
		if errors.Is(err, admin.ErrRouteAlreadyExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.CreateRouteResponse{Route: routeToProto(route), AppliedAt: timestamppb.Now()}), nil
}

func (s *Server) RemoveRoute(ctx context.Context, req *connect.Request[apiv1.RemoveRouteRequest]) (*connect.Response[apiv1.RemoveRouteResponse], error) {
	if err := s.cfg.Admin.RemoveRoute(ctx, req.Msg.GetRouteId()); err != nil {
		if errors.Is(err, admin.ErrRouteNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.RemoveRouteResponse{AppliedAt: timestamppb.Now()}), nil
}

// ProvisionDevice creates a new device from a template, returning its
// one-time-display Mosquitto password. Error codes are more granular here
// than DeviceService's (which collapses most failures to InvalidArgument
// as a deliberate MVP simplification for its small set of cases): a
// mutating, privileged operation has genuinely distinct failure classes
// worth a client acting differently on - CodeAlreadyExists for a
// duplicate id, CodeInvalidArgument for a bad id or unknown template, and
// CodeInternal for anything operational (the provisioning script,
// gateway.yaml write, or systemctl) that the caller cannot fix by changing
// the request.
func (s *Server) ProvisionDevice(ctx context.Context, req *connect.Request[apiv1.ProvisionDeviceRequest]) (*connect.Response[apiv1.ProvisionDeviceResponse], error) {
	id := req.Msg.GetDeviceId()
	template := req.Msg.GetTemplate()

	device, password, err := s.cfg.Admin.ProvisionDevice(ctx, id, template)
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrDeviceAlreadyExists):
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		case errors.Is(err, admin.ErrUnknownTemplate), errors.Is(err, admin.ErrInvalidDeviceID):
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	return connect.NewResponse(&apiv1.ProvisionDeviceResponse{
		Device:       deviceToProto(device),
		MqttUsername: device.ID,
		MqttPassword: password,
		AppliedAt:    timestamppb.Now(),
	}), nil
}

// ProvisionCYD delivers the newly generated MQTT identity directly to the
// first-boot CYD. The API intentionally has no MQTT password field: that
// secret is never visible to gateway-web or persisted by the registry.
func (s *Server) ProvisionCYD(ctx context.Context, req *connect.Request[apiv1.ProvisionCYDRequest]) (*connect.Response[apiv1.ProvisionCYDResponse], error) {
	device, address, err := s.cfg.Admin.ProvisionCYD(ctx, req.Msg.GetDeviceId(), req.Msg.GetDeviceIp())
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrDeviceAlreadyExists):
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		case errors.Is(err, admin.ErrInvalidDeviceID), errors.Is(err, admin.ErrInvalidDeviceAddress):
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		case errors.Is(err, admin.ErrDeviceNotProvisionable):
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&apiv1.ProvisionCYDResponse{
		Device: deviceToProto(device), DeviceIp: address, AppliedAt: timestamppb.Now(),
	}), nil
}

// ProvisionLED uses the same direct-to-NVS first-boot protocol as the CYD.
// Keeping the generated password out of this response ensures gateway-web
// never handles an MQTT secret for either ESP family.
func (s *Server) ProvisionLED(ctx context.Context, req *connect.Request[apiv1.ProvisionLEDRequest]) (*connect.Response[apiv1.ProvisionLEDResponse], error) {
	device, address, err := s.cfg.Admin.ProvisionLED(ctx, req.Msg.GetDeviceId(), req.Msg.GetDeviceIp())
	if err != nil {
		switch {
		case errors.Is(err, admin.ErrDeviceAlreadyExists):
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		case errors.Is(err, admin.ErrInvalidDeviceID), errors.Is(err, admin.ErrInvalidDeviceAddress):
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		case errors.Is(err, admin.ErrDeviceNotProvisionable):
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&apiv1.ProvisionLEDResponse{
		Device: deviceToProto(device), DeviceIp: address, AppliedAt: timestamppb.Now(),
	}), nil
}

// SetDeviceEnabled toggles a device's enabled field. CodeNotFound for an
// unknown device, CodeInternal for a restart failure - see
// admin.Server.SetDeviceEnabled's doc comment for why a restart failure
// does not undo the YAML change.
func (s *Server) SetDeviceEnabled(ctx context.Context, req *connect.Request[apiv1.SetDeviceEnabledRequest]) (*connect.Response[apiv1.SetDeviceEnabledResponse], error) {
	id := req.Msg.GetDeviceId()

	device, err := s.cfg.Admin.SetDeviceEnabled(ctx, id, req.Msg.GetEnabled())
	if err != nil {
		if errors.Is(err, admin.ErrDeviceNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&apiv1.SetDeviceEnabledResponse{
		Device:    deviceToProto(device),
		AppliedAt: timestamppb.Now(),
	}), nil
}

// RemoveDevice revokes a device's credential and removes it from
// gateway.yaml. CodeNotFound for an unknown device, CodeInternal for
// anything else (script/systemctl failures) - see
// admin.Server.RemoveDevice's doc comment for why there is no rollback on
// a partial failure here, unlike ProvisionDevice.
func (s *Server) RemoveDevice(ctx context.Context, req *connect.Request[apiv1.RemoveDeviceRequest]) (*connect.Response[apiv1.RemoveDeviceResponse], error) {
	id := req.Msg.GetDeviceId()

	if err := s.cfg.Admin.RemoveDevice(ctx, id); err != nil {
		if errors.Is(err, admin.ErrDeviceNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	return connect.NewResponse(&apiv1.RemoveDeviceResponse{
		AppliedAt: timestamppb.Now(),
	}), nil
}

func inconsistencyToProto(item registry.Inconsistency) *apiv1.Inconsistency {
	return &apiv1.Inconsistency{
		Id: item.ID, Kind: item.Kind, DeviceId: item.DeviceID,
		Cause: item.Cause, CompensationError: item.CompensationError,
		CreatedAt: timestamppb.New(item.CreatedAt),
	}
}

// ListInconsistencies is read-only and payload-free by construction
// (Inconsistency carries only error strings, never a device secret or MQTT
// payload) - safe to answer without the granular error-code treatment
// ProvisionDevice/ProvisionCYD above need.
func (s *Server) ListInconsistencies(ctx context.Context, _ *connect.Request[apiv1.ListInconsistenciesRequest]) (*connect.Response[apiv1.ListInconsistenciesResponse], error) {
	items, err := s.cfg.Admin.ListInconsistencies(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response := &apiv1.ListInconsistenciesResponse{Inconsistencies: make([]*apiv1.Inconsistency, 0, len(items))}
	for _, item := range items {
		response.Inconsistencies = append(response.Inconsistencies, inconsistencyToProto(item))
	}
	return connect.NewResponse(response), nil
}

// ResolveInconsistency only marks the entry resolved (registry.Store.
// ResolveInconsistency's doc comment) - it never touches the registry or
// the broker itself.
func (s *Server) ResolveInconsistency(ctx context.Context, req *connect.Request[apiv1.ResolveInconsistencyRequest]) (*connect.Response[apiv1.ResolveInconsistencyResponse], error) {
	if err := s.cfg.Admin.ResolveInconsistency(ctx, req.Msg.GetId()); err != nil {
		if errors.Is(err, admin.ErrInconsistencyNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.ResolveInconsistencyResponse{}), nil
}
