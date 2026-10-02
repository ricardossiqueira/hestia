package apigateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// DeviceV2API is the public JSON contract used by gateway-web. It contains no
// secret-bearing provision request or broker password. The root/admin process
// supplies a discovery inbox populated by its mDNS adapter.
type DeviceV2API struct {
	Inbox    *devicev2.Inbox
	Registry *registry.Store
}

func (a DeviceV2API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		v2Error(w, http.StatusMethodNotAllowed, "POST is required")
		return
	}
	switch r.URL.Path {
	case "/iot.gateway.api.v2.DevicePlatformService/ListDiscovery":
		a.listDiscovery(w)
	case "/iot.gateway.api.v2.DevicePlatformService/RegisterDiscoveredDevice":
		a.register(r.Context(), w, r)
	case "/iot.gateway.api.v2.DevicePlatformService/ListDevices":
		a.listDevices(r.Context(), w)
	case "/iot.gateway.api.v2.DevicePlatformService/GetDevice":
		a.getDevice(r.Context(), w, r)
	case "/iot.gateway.api.v2.DevicePlatformService/ListAutomationRules":
		a.listAutomationRules(r.Context(), w)
	case "/iot.gateway.api.v2.DevicePlatformService/CreateAutomationRule":
		a.createAutomationRule(r.Context(), w, r)
	default:
		v2Error(w, http.StatusNotFound, "unknown v2 method")
	}
}
func (a DeviceV2API) listDiscovery(w http.ResponseWriter) {
	if a.Inbox == nil {
		v2Error(w, http.StatusServiceUnavailable, "discovery is unavailable")
		return
	}
	items := a.Inbox.List()
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, v2Discovery(item))
	}
	v2JSON(w, http.StatusOK, map[string]any{"entries": out})
}
func (a DeviceV2API) register(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceUID string `json:"deviceUid"`
		DeviceID  string `json:"deviceId"`
	}
	if !v2Decode(r, &request) {
		v2Error(w, http.StatusBadRequest, "invalid registration request")
		return
	}
	if a.Inbox == nil || a.Registry == nil {
		v2Error(w, http.StatusServiceUnavailable, "device platform is unavailable")
		return
	}
	var found *devicev2.DiscoveryEntry
	for _, item := range a.Inbox.List() {
		if item.DeviceUID == request.DeviceUID {
			copy := item
			found = &copy
			break
		}
	}
	if found == nil {
		v2Error(w, http.StatusNotFound, "discovered device not found")
		return
	}
	if found.State != devicev2.ReadyToRegister || found.Info == nil {
		v2Error(w, http.StatusConflict, "device is not ready to register; complete pairing first")
		return
	}
	manifest, canonical, hash, err := devicev2.Parse(string(found.Info.Manifest))
	if err != nil || hash != found.ManifestSHA256 {
		v2Error(w, http.StatusBadRequest, "invalid discovered manifest")
		return
	}
	accepted, err := a.Registry.AcceptV2Manifest(ctx, canonical, fingerprint(found.Info.IdentityPublicKey))
	if err != nil {
		v2Error(w, http.StatusInternalServerError, err.Error())
		return
	}
	device, err := a.Registry.RegisterV2Device(ctx, registry.V2Device{DeviceID: request.DeviceID, DeviceUID: found.DeviceUID, ManifestID: manifest.ManifestID, ManifestRevision: accepted.Revision, ManifestSHA256: accepted.SHA256, FirmwareVersion: found.Info.FirmwareVersion, IdentityPublicKey: found.Info.IdentityPublicKey})
	if err != nil {
		v2Error(w, http.StatusBadRequest, err.Error())
		return
	}
	a.Inbox.MarkRegistered(found.DeviceUID)
	v2JSON(w, http.StatusOK, map[string]any{"device": v2Device(device, manifest), "steps": []any{map[string]string{"id": "pairing", "label": "Pairing e prova de identidade", "status": "complete"}, map[string]string{"id": "manifest", "label": "Manifest validado", "status": "complete"}, map[string]string{"id": "binding", "label": "Binding e ACL derivada", "status": "complete"}, map[string]string{"id": "provisioning", "label": "Configuração MQTT aguardando transporte cifrado", "status": "pending"}, map[string]string{"id": "activation", "label": "Ativação MQTT", "status": "pending"}}})
}
func (a DeviceV2API) listDevices(ctx context.Context, w http.ResponseWriter) {
	if a.Registry == nil {
		v2Error(w, http.StatusServiceUnavailable, "device platform is unavailable")
		return
	}
	items, err := a.Registry.ListV2Devices(ctx)
	if err != nil {
		v2Error(w, 500, err.Error())
		return
	}
	out := make([]any, 0, len(items))
	for _, item := range items {
		m, ok, err := a.Registry.ResolveV2Manifest(ctx, item.DeviceID)
		if err != nil || !ok {
			v2Error(w, 500, "bound manifest missing")
			return
		}
		out = append(out, v2Device(item, m))
	}
	v2JSON(w, 200, map[string]any{"devices": out})
}
func (a DeviceV2API) getDevice(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var request struct {
		DeviceID string `json:"deviceId"`
	}
	if !v2Decode(r, &request) {
		v2Error(w, 400, "invalid device request")
		return
	}
	d, err := a.Registry.GetV2Device(ctx, request.DeviceID)
	if errors.Is(err, registry.ErrV2DeviceNotFound) {
		v2Error(w, 404, "device not found")
		return
	}
	if err != nil {
		v2Error(w, 500, err.Error())
		return
	}
	m, ok, err := a.Registry.ResolveV2Manifest(ctx, d.DeviceID)
	if err != nil || !ok {
		v2Error(w, 500, "bound manifest missing")
		return
	}
	v2JSON(w, 200, v2Device(d, m))
}

func (a DeviceV2API) listAutomationRules(ctx context.Context, w http.ResponseWriter) {
	if a.Registry == nil {
		v2Error(w, http.StatusServiceUnavailable, "device platform is unavailable")
		return
	}
	rules, err := a.Registry.ListV2AutomationRules(ctx)
	if err != nil {
		v2Error(w, 500, err.Error())
		return
	}
	out := make([]any, 0, len(rules))
	for _, rule := range rules {
		out = append(out, v2Rule(rule))
	}
	v2JSON(w, 200, map[string]any{"rules": out})
}
func (a DeviceV2API) createAutomationRule(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	var request struct {
		Rule struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
			Trigger struct {
				SourceDeviceID string `json:"sourceDeviceId"`
				OutputChannel  string `json:"outputChannel"`
				EventType      string `json:"eventType"`
				IgnoreRetained bool   `json:"ignoreRetained"`
				ConditionJSON  string `json:"conditionJson"`
			} `json:"trigger"`
			Action struct {
				TargetDeviceID string          `json:"targetDeviceId"`
				CommandType    string          `json:"commandType"`
				Parameters     json.RawMessage `json:"parameters"`
			} `json:"action"`
		} `json:"rule"`
	}
	if !v2Decode(r, &request) {
		v2Error(w, 400, "invalid automation rule")
		return
	}
	parameters := request.Rule.Action.Parameters
	if len(parameters) == 0 {
		parameters = []byte(`{}`)
	}
	rule, err := a.Registry.CreateV2AutomationRule(ctx, registry.V2AutomationRule{ID: request.Rule.ID, Enabled: request.Rule.Enabled, SourceDeviceID: request.Rule.Trigger.SourceDeviceID, OutputChannel: request.Rule.Trigger.OutputChannel, EventType: request.Rule.Trigger.EventType, IgnoreRetained: request.Rule.Trigger.IgnoreRetained, ConditionJSON: request.Rule.Trigger.ConditionJSON, TargetDeviceID: request.Rule.Action.TargetDeviceID, CommandType: request.Rule.Action.CommandType, ParametersJSON: string(parameters)})
	if err != nil {
		v2Error(w, 400, err.Error())
		return
	}
	v2JSON(w, 200, map[string]any{"rule": v2Rule(rule)})
}
func v2Rule(rule registry.V2AutomationRule) map[string]any {
	var parameters any
	_ = json.Unmarshal([]byte(rule.ParametersJSON), &parameters)
	trigger := map[string]any{"sourceDeviceId": rule.SourceDeviceID, "outputChannel": rule.OutputChannel, "conditionJson": rule.ConditionJSON}
	if rule.EventType != "" {
		trigger["eventType"] = rule.EventType
	}
	if rule.OutputChannel == "state" {
		trigger["ignoreRetained"] = rule.IgnoreRetained
	}
	return map[string]any{"id": rule.ID, "enabled": rule.Enabled, "trigger": trigger, "action": map[string]any{"targetDeviceId": rule.TargetDeviceID, "commandType": rule.CommandType, "parameters": parameters}, "updatedAt": rule.UpdatedAt.Format(time.RFC3339)}
}
func v2Discovery(item devicev2.DiscoveryEntry) map[string]any {
	out := map[string]any{"deviceUid": item.DeviceUID, "address": item.Host, "port": item.Port, "model": item.Model, "firmwareVersion": item.Firmware, "manifestSha256": item.ManifestSHA256, "status": item.State, "pairingRequired": item.Pairing, "lastSeenAt": item.SeenAt.Format(time.RFC3339), "trust": "unknown"}
	if item.Error != "" {
		out["diagnostic"] = item.Error
	}
	if item.Info != nil {
		out["identityFingerprint"] = fingerprint(item.Info.IdentityPublicKey)
		if m, _, _, err := devicev2.Parse(string(item.Info.Manifest)); err == nil {
			out["manifest"] = m
		}
	}
	return out
}
func v2Device(d registry.V2Device, m devicev2.Manifest) map[string]any {
	return map[string]any{"deviceId": d.DeviceID, "deviceUid": d.DeviceUID, "firmwareVersion": d.FirmwareVersion, "manifest": m, "manifestHash": d.ManifestSHA256, "manifestRevision": strconv.FormatUint(d.ManifestRevision, 10), "desiredState": d.DesiredState, "activeState": v2Active(d.ActiveState), "identityFingerprint": fingerprint(d.IdentityPublicKey), "updatedAt": d.UpdatedAt.Format(time.RFC3339)}
}
func v2Active(state string) string {
	switch state {
	case "active", "offline", "pending", "failed":
		return state
	}
	return "pending"
}
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "SHA256:" + hex.EncodeToString(sum[:8])
}
func v2Decode(r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<10))
	d.DisallowUnknownFields()
	return d.Decode(target) == nil
}
func v2JSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func v2Error(w http.ResponseWriter, status int, message string) {
	v2JSON(w, status, map[string]string{"message": message})
}
