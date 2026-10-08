package apigateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apiv1 "github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1"
	"github.com/ricardossiqueira/iot-gateway/api/gen/go/iot/gateway/api/v1/apiv1connect"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

func statusTestServer(t *testing.T, platform *DeviceV2API, runtimeError error) *httptest.Server {
	t.Helper()
	backend := httptest.NewServer(connect.NewUnaryHandler(apiv1connect.GatewayServiceGetStatusProcedure,
		func(context.Context, *connect.Request[apiv1.GetStatusRequest]) (*connect.Response[apiv1.GetStatusResponse], error) {
			if runtimeError != nil {
				return nil, runtimeError
			}
			return connect.NewResponse(&apiv1.GetStatusResponse{Started: true, MqttConnected: true, Subscriptions: 5, AcceptedMessages: 474, RejectedMessages: 3}), nil
		}))
	t.Cleanup(backend.Close)
	server, err := New(Config{Address: "127.0.0.1:0", InternalAPIURL: backend.URL, Credentials: Credentials{Username: "user", Password: "pass"}, AllowedOrigins: []string{"http://localhost:5173"}, DeviceV2: platform}, nil)
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewUnstartedServer(server.http.Handler)
	public.EnableHTTP2 = true
	public.StartTLS()
	t.Cleanup(public.Close)
	return public
}

func statusTestPlatform(t *testing.T) *DeviceV2API {
	t.Helper()
	store, err := registry.Open(context.Background(), filepath.Join(t.TempDir(), "registry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &DeviceV2API{Registry: store, Inbox: devicev2.NewInbox(90 * time.Second)}
}

func TestPublicGetStatusBreakdownMatchesV2Lists(t *testing.T) {
	a := statusTestPlatform(t)
	// All registered devices can remain active even when discovery reports one offline.
	for index, state := range []string{"active", "active", "active", "offline", "failed", "unprovisioned"} {
		id := fmt.Sprintf("device-%d", index)
		registerTestV2Device(t, a.Registry, id)
		if _, err := a.Registry.SetV2DeviceState(context.Background(), id, "active", state); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 3 {
		seen := time.Now()
		if index == 2 {
			seen = seen.Add(-2 * time.Minute)
		}
		uid := fmt.Sprintf("uid-device-%d", index)
		if err := a.Inbox.Observe(devicev2.Announcement{DeviceUID: uid, Host: "192.0.2.10", Port: 8080, Protocol: "iot-device-v1", ManifestSHA256: strings.Repeat("a", 64), SeenAt: seen}); err != nil {
			t.Fatal(err)
		}
		a.Inbox.MarkInspected(uid, devicev2.DeviceInfo{PairingRequired: false}, nil)
	}
	ts := statusTestServer(t, a, nil)
	for name, options := range map[string][]connect.ClientOption{"connect-json": {connect.WithProtoJSON()}, "connect-proto": {}, "grpc": {connect.WithGRPC()}, "grpc-web": {connect.WithGRPCWeb()}} {
		t.Run(name, func(t *testing.T) {
			client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL, options...)
			req := connect.NewRequest(&apiv1.GetStatusRequest{})
			req.Header().Set("Authorization", authHeader("user", "pass"))
			resp, err := client.GetStatus(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.AcceptedMessages != 474 || resp.Msg.RejectedMessages != 3 || resp.Msg.Subscriptions != 5 || !resp.Msg.MqttConnected {
				t.Fatalf("lost runtime fields: %v", resp.Msg)
			}
			if resp.Msg.Devices.Total != 6 || resp.Msg.Devices.ByActiveState["active"] != 3 || resp.Msg.Devices.ByActiveState["pending"] != 1 || resp.Msg.Devices.ByActiveState["failed"] != 1 || resp.Msg.Devices.ByActiveState["offline"] != 1 {
				t.Fatalf("devices: %v", resp.Msg.Devices)
			}
			if resp.Msg.Discovery.Total != 3 || resp.Msg.Discovery.Online != 2 || resp.Msg.Discovery.Offline != 1 || resp.Msg.Discovery.ByStatus["ready_to_register"] != 2 {
				t.Fatalf("discovery: %v", resp.Msg.Discovery)
			}
			// Compare to the actual list handlers, not a separate approximation.
			devicesResponse := httptest.NewRecorder()
			a.listDevices(context.Background(), devicesResponse)
			var listed struct {
				Devices []struct {
					ActiveState string `json:"activeState"`
				} `json:"devices"`
			}
			if err := json.Unmarshal(devicesResponse.Body.Bytes(), &listed); err != nil {
				t.Fatal(err)
			}
			counts := map[string]uint32{}
			for _, device := range listed.Devices {
				counts[device.ActiveState]++
			}
			for state, count := range resp.Msg.Devices.ByActiveState {
				if counts[state] != count {
					t.Fatalf("state %s: list=%d status=%d", state, counts[state], count)
				}
			}
			if int(resp.Msg.Discovery.Total) != len(a.Inbox.List()) {
				t.Fatal("discovery totals disagree")
			}
		})
	}
}

func TestPublicGetStatusEmptyAndErrors(t *testing.T) {
	for _, scenario := range []string{"empty", "closed-registry", "missing-inbox", "runtime-error"} {
		t.Run(scenario, func(t *testing.T) {
			a := statusTestPlatform(t)
			var runtimeError error
			wantCode := connect.CodeUnknown
			switch scenario {
			case "closed-registry":
				_ = a.Registry.Close()
				wantCode = connect.CodeInternal
			case "missing-inbox":
				a.Inbox = nil
				wantCode = connect.CodeUnavailable
			case "runtime-error":
				runtimeError = connect.NewError(connect.CodeUnavailable, errors.New("runtime unavailable"))
				wantCode = connect.CodeUnavailable
			}
			ts := statusTestServer(t, a, runtimeError)
			client := apiv1connect.NewGatewayServiceClient(ts.Client(), ts.URL)
			req := connect.NewRequest(&apiv1.GetStatusRequest{})
			req.Header().Set("Authorization", authHeader("user", "pass"))
			resp, err := client.GetStatus(context.Background(), req)
			if scenario != "empty" {
				if connect.CodeOf(err) != wantCode {
					t.Fatalf("error=%v, want %v", err, wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Devices == nil || resp.Msg.Discovery == nil || resp.Msg.Devices.Total != 0 || resp.Msg.Discovery.Total != 0 || len(resp.Msg.Devices.ByActiveState) != 4 || len(resp.Msg.Discovery.ByStatus) != 7 {
				t.Fatalf("empty breakdown missing: %v", resp.Msg)
			}
		})
	}
}

func TestPublicGetStatusJSONAuthAndCORS(t *testing.T) {
	ts := statusTestServer(t, statusTestPlatform(t), nil)
	for _, authenticated := range []bool{false, true} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+apiv1connect.GatewayServiceGetStatusProcedure, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "http://localhost:5173")
		if authenticated {
			req.SetBasicAuth("user", "pass")
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if !authenticated {
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			continue
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" {
			t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if string(payload["acceptedMessages"]) != `"474"` || payload["devices"] == nil || payload["discovery"] == nil {
			t.Fatalf("JSON contract=%s", payload)
		}
	}
}
