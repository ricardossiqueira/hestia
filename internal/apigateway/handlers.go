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
		Device:      deviceToProto(device),
		AppliedAt:   timestamppb.Now(),
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
