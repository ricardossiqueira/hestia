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

func manifestToProto(manifest registry.DeviceManifest) *apiv1.DeviceManifest {
	return &apiv1.DeviceManifest{
		Id: manifest.ID, DisplayName: manifest.DisplayName, Revision: manifest.Revision,
		DocumentJson: manifest.Document, CreatedBy: manifest.CreatedBy,
		CreatedAt: timestamppb.New(manifest.CreatedAt),
	}
}

func manifestBindingToProto(binding registry.DeviceManifestBinding) *apiv1.DeviceManifestBinding {
	return &apiv1.DeviceManifestBinding{
		DeviceId: binding.DeviceID, ManifestId: binding.ManifestID, ManifestRevision: binding.ManifestRevision,
	}
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

// ListDeviceManifests returns the published catalog for the future textual
// editor and generic provisioning form. It intentionally never exposes a
// draft, because only published definitions may change runtime policy.
func (s *Server) ListDeviceManifests(ctx context.Context, _ *connect.Request[apiv1.ListDeviceManifestsRequest]) (*connect.Response[apiv1.ListDeviceManifestsResponse], error) {
	manifests, err := s.cfg.Admin.ListPublishedDeviceManifests(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response := &apiv1.ListDeviceManifestsResponse{Manifests: make([]*apiv1.DeviceManifest, 0, len(manifests))}
	for _, manifest := range manifests {
		response.Manifests = append(response.Manifests, manifestToProto(manifest))
	}
	return connect.NewResponse(response), nil
}

func (s *Server) GetDeviceManifest(ctx context.Context, req *connect.Request[apiv1.GetDeviceManifestRequest]) (*connect.Response[apiv1.GetDeviceManifestResponse], error) {
	manifest, err := s.cfg.Admin.GetPublishedDeviceManifest(ctx, req.Msg.GetManifestId())
	if errors.Is(err, registry.ErrManifestNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&apiv1.GetDeviceManifestResponse{Manifest: manifestToProto(manifest)}), nil
}

// ListDeviceManifestBindings lets the Web show the immutable source used for
// each device, without exposing any provisioning secret or broker detail.
func (s *Server) ListDeviceManifestBindings(ctx context.Context, _ *connect.Request[apiv1.ListDeviceManifestBindingsRequest]) (*connect.Response[apiv1.ListDeviceManifestBindingsResponse], error) {
	bindings, err := s.cfg.Admin.ListDeviceManifestBindings(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	response := &apiv1.ListDeviceManifestBindingsResponse{Bindings: make([]*apiv1.DeviceManifestBinding, 0, len(bindings))}
	for _, binding := range bindings {
		response.Bindings = append(response.Bindings, manifestBindingToProto(binding))
	}
	return connect.NewResponse(response), nil
}

func (s *Server) MigrateDeviceToManifest(ctx context.Context, req *connect.Request[apiv1.MigrateDeviceToManifestRequest]) (*connect.Response[apiv1.MigrateDeviceToManifestResponse], error) {
	device, err := s.cfg.Admin.MigrateDeviceToManifest(ctx, req.Msg.GetDeviceId(), req.Msg.GetManifestId(), authenticatedActor(ctx))
	if errors.Is(err, registry.ErrDeviceNotFound) || errors.Is(err, registry.ErrManifestNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&apiv1.MigrateDeviceToManifestResponse{Device: deviceToProto(device), AppliedAt: timestamppb.Now()}), nil
}

func (s *Server) CreateDeviceManifestDraft(ctx context.Context, req *connect.Request[apiv1.CreateDeviceManifestDraftRequest]) (*connect.Response[apiv1.CreateDeviceManifestDraftResponse], error) {
	manifest, err := s.cfg.Admin.CreateDeviceManifestDraft(ctx, req.Msg.GetDocumentJson(), authenticatedActor(ctx))
	if err != nil {
		if errors.Is(err, registry.ErrManifestAlreadyExists) {
			return nil, connect.NewError(connect.CodeAlreadyExists, err)
		}
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.CreateDeviceManifestDraftResponse{Manifest: manifestToProto(manifest)}), nil
}

func (s *Server) CreateDeviceManifestRevisionDraft(ctx context.Context, req *connect.Request[apiv1.CreateDeviceManifestRevisionDraftRequest]) (*connect.Response[apiv1.CreateDeviceManifestRevisionDraftResponse], error) {
	manifest, err := s.cfg.Admin.CreateDeviceManifestRevisionDraft(ctx, req.Msg.GetManifestId(), req.Msg.GetDocumentJson(), authenticatedActor(ctx))
	if errors.Is(err, registry.ErrManifestNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.CreateDeviceManifestRevisionDraftResponse{Manifest: manifestToProto(manifest)}), nil
}

func (s *Server) PublishDeviceManifest(ctx context.Context, req *connect.Request[apiv1.PublishDeviceManifestRequest]) (*connect.Response[apiv1.PublishDeviceManifestResponse], error) {
	manifest, err := s.cfg.Admin.PublishDeviceManifest(ctx, req.Msg.GetManifestId(), req.Msg.GetRevision(), authenticatedActor(ctx))
	if errors.Is(err, registry.ErrManifestNotFound) || errors.Is(err, registry.ErrManifestRevisionNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if errors.Is(err, registry.ErrManifestRevisionNotDraft) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&apiv1.PublishDeviceManifestResponse{Manifest: manifestToProto(manifest)}), nil
}

// ProvisionDeviceByIP is the manifest-driven successor to the retired
// ProvisionCYD/ProvisionLED RPCs. It never exposes the one-time MQTT
// password to its caller.
func (s *Server) ProvisionDeviceByIP(ctx context.Context, req *connect.Request[apiv1.ProvisionDeviceByIPRequest]) (*connect.Response[apiv1.ProvisionDeviceByIPResponse], error) {
	device, address, err := s.cfg.Admin.ProvisionDeviceByIP(ctx, req.Msg.GetDeviceId(), req.Msg.GetManifestId(), req.Msg.GetDeviceIp())
	if err != nil {
		switch {
		case errors.Is(err, registry.ErrManifestNotFound):
			return nil, connect.NewError(connect.CodeNotFound, err)
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
	return connect.NewResponse(&apiv1.ProvisionDeviceByIPResponse{
		Device: deviceToProto(device), DeviceIp: address, ManifestId: req.Msg.GetManifestId(), AppliedAt: timestamppb.Now(),
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
// ProvisionDeviceByIP above needs.
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
