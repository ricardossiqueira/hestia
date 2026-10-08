package apigateway

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
)

// getStatus aggregates at the public edge: only the admin process owns the
// discovery inbox, while the sandboxed process owns the live MQTT counters.
func (s *Server) getStatus(ctx context.Context, req *connect.Request[apiv1.GetStatusRequest]) (*connect.Response[apiv1.GetStatusResponse], error) {
	a := s.cfg.DeviceV2
	if a.Registry == nil || a.Inbox == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("device status sources are unavailable"))
	}
	devices, err := a.Registry.ListV2Devices(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("read device status: %w", err))
	}
	response, err := s.runtime.GetStatus(ctx, req)
	if err != nil {
		return nil, err
	}
	registered := &apiv1.DeviceBreakdown{
		Total:         uint32(len(devices)),
		ByActiveState: map[string]uint32{"active": 0, "offline": 0, "pending": 0, "failed": 0},
	}
	for _, device := range devices {
		registered.ByActiveState[v2Active(device.ActiveState)]++
	}
	entries := a.Inbox.List()
	discovery := &apiv1.DiscoveryBreakdown{
		Total: uint32(len(entries)),
		ByStatus: map[string]uint32{
			"seen": 0, "inspected": 0, "pairing_required": 0, "ready_to_register": 0,
			"registered": 0, "rejected": 0, "offline": 0,
		},
	}
	for _, entry := range entries {
		discovery.ByStatus[string(entry.State)]++
		if entry.State == devicev2.Offline {
			discovery.Offline++
		} else {
			discovery.Online++
		}
	}
	response.Msg.Devices = registered
	response.Msg.Discovery = discovery
	return response, nil
}
