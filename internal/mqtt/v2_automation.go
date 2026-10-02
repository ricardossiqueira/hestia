package mqtt

// This file is intentionally a separate runtime from the legacy config based
// router in gateway.go. Device Platform v2 derives every topic and permission
// from an accepted manifest binding; consulting a v1 config entry here would
// reintroduce an unvalidated raw-topic escape hatch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/automationrule"
	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
	"github.com/ricardossiqueira/iot-gateway/internal/registry"
)

// V2AutomationStore is the persistence boundary for the v2 runtime. The
// registry implementation is authoritative for active bindings, rules and
// execution reservations; the runtime deliberately keeps no rule cache.
type V2AutomationStore interface {
	ListV2RuntimeDevices(context.Context) ([]registry.V2RuntimeDevice, error)
	ListV2AutomationRules(context.Context) ([]registry.V2AutomationRule, error)
	ReserveV2AutomationExecution(context.Context, string, string, string, string) (bool, error)
	CompleteV2AutomationExecution(context.Context, string, string, bool) error
	CountV2PublishedAutomationExecutionsSince(context.Context, string, time.Time) (int, error)
}

type v2Route struct {
	deviceID string
	channel  string
	manifest devicev2.Manifest
}

// EnableV2Runtime loads the currently active manifest bindings and installs
// subscriptions. It is safe before or after Start; callers should invoke it
// again after provisioning activates/deactivates a v2 device.
func (g *Gateway) EnableV2Runtime(ctx context.Context, store V2AutomationStore) error {
	if store == nil {
		return errors.New("v2 automation store is required")
	}
	routes, topics, devices, err := buildV2Routes(ctx, store)
	if err != nil {
		return err
	}
	g.mu.Lock()
	started, closed := g.started, g.closed
	g.mu.Unlock()
	if closed {
		return errors.New("MQTT gateway is closed")
	}
	g.configMu.Lock()
	for topic := range routes {
		if _, collision := g.routes[topic]; collision {
			g.configMu.Unlock()
			return fmt.Errorf("v2 topic %q collides with legacy MQTT policy", topic)
		}
	}
	old := make(map[string]struct{}, len(g.v2Topics))
	for _, topic := range g.v2Topics {
		old[topic] = struct{}{}
	}
	if started {
		for _, topic := range topics {
			if _, exists := old[topic]; exists {
				continue
			}
			if err := g.client.Subscribe(ctx, topic, g.handleV2Message); err != nil {
				g.configMu.Unlock()
				return fmt.Errorf("subscribe to v2 %q: %w", topic, err)
			}
		}
	}
	g.v2Store, g.v2Routes, g.v2Topics, g.v2Devices = store, routes, topics, devices
	g.configMu.Unlock()
	return nil
}

func buildV2Routes(ctx context.Context, store V2AutomationStore) (map[string]v2Route, []string, map[string]devicev2.Manifest, error) {
	devices, err := store.ListV2RuntimeDevices(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load v2 runtime devices: %w", err)
	}
	routes := make(map[string]v2Route)
	manifests := make(map[string]devicev2.Manifest, len(devices))
	for _, device := range devices {
		manifests[device.Device.DeviceID] = device.Manifest
		for _, output := range device.Manifest.MQTT.Publish {
			topic := "devices/" + device.Device.DeviceID + "/" + output.Channel
			if previous, exists := routes[topic]; exists {
				return nil, nil, nil, fmt.Errorf("v2 topic %q is assigned to both %s and %s", topic, previous.deviceID, device.Device.DeviceID)
			}
			routes[topic] = v2Route{deviceID: device.Device.DeviceID, channel: output.Channel, manifest: device.Manifest}
		}
	}
	topics := make([]string, 0, len(routes))
	for topic := range routes {
		topics = append(topics, topic)
	}
	sort.Strings(topics)
	return routes, topics, manifests, nil
}

func (g *Gateway) handleV2Message(ctx context.Context, topic string, payload []byte, retained bool) {
	g.configMu.RLock()
	route, exists := g.v2Routes[topic]
	store := g.v2Store
	g.configMu.RUnlock()
	if !exists || store == nil {
		return
	}
	message, err := validateV2Inbound(route, topic, payload, retained)
	if err != nil {
		g.rejectedMessages.Add(1)
		g.logger.Rejected(ctx, RejectedMessage{DeviceID: route.deviceID, Kind: Kind(route.channel), Topic: topic, Reason: err.Error()})
		g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: route.deviceID, Kind: Kind(route.channel), Topic: topic, Outcome: "rejected", Detail: err.Error()})
		return
	}
	g.acceptedMessages.Add(1)
	g.logger.Accepted(ctx, message)
	g.recordEvent(ActivityEvent{Timestamp: message.Timestamp, DeviceID: message.DeviceID, Kind: message.Kind, Topic: topic, Outcome: "accepted"})
	g.fireV2AutomationRules(ctx, store, route, message)
}

func validateV2Inbound(route v2Route, topic string, payload []byte, retained bool) (Message, error) {
	channel := route.channel
	if channel != "state" && retained {
		return Message{}, fmt.Errorf("%s messages must not be retained", channel)
	}
	if channel == "state" && !retained {
		// A live state update is valid and can trigger a rule. The manifest's
		// retained=true constrains publisher behavior but does not make the
		// broker flag a correctness condition for a particular delivery.
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return Message{}, errors.New("payload must be a JSON object")
	}
	var envelope struct {
		MessageID string `json:"message_id"`
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return Message{}, fmt.Errorf("decode output envelope: %w", err)
	}
	if !uuidPattern.MatchString(envelope.MessageID) {
		return Message{}, errors.New("message_id must be a UUID")
	}
	if !strings.HasSuffix(envelope.Timestamp, "Z") {
		return Message{}, errors.New("timestamp must use UTC (Z)")
	}
	timestamp, err := time.Parse(time.RFC3339, envelope.Timestamp)
	if err != nil {
		return Message{}, fmt.Errorf("timestamp must be RFC3339: %w", err)
	}
	delete(object, "message_id")
	delete(object, "timestamp")
	var schema json.RawMessage
	if channel == "event" {
		event, ok := route.manifest.Event(envelope.Type)
		if !ok {
			return Message{}, fmt.Errorf("event type %q is not declared", envelope.Type)
		}
		delete(object, "type")
		schema = event.Payload
	} else {
		output, ok := route.manifest.Output(channel)
		if !ok {
			return Message{}, errors.New("output channel is not declared")
		}
		schema = output.Schema
	}
	fields, err := json.Marshal(object)
	if err != nil {
		return Message{}, err
	}
	if _, err := devicev2.ValidateFields(schema, fields); err != nil {
		return Message{}, fmt.Errorf("output schema: %w", err)
	}
	return Message{DeviceID: route.deviceID, Kind: Kind(channel), Topic: topic, MessageID: envelope.MessageID, Timestamp: timestamp, Payload: append([]byte(nil), payload...), Retained: retained}, nil
}

func (g *Gateway) fireV2AutomationRules(ctx context.Context, store V2AutomationStore, route v2Route, message Message) {
	rules, err := store.ListV2AutomationRules(ctx)
	if err != nil {
		g.v2Failure(ctx, "rule_match", err)
		return
	}
	for _, rule := range rules {
		if !rule.Enabled || rule.SourceDeviceID != message.DeviceID || rule.OutputChannel != route.channel {
			continue
		}
		if route.channel == "event" && rule.EventType != eventPayloadType(message.Payload) {
			continue
		}
		if route.channel == "state" && message.Retained && rule.IgnoreRetained {
			continue
		}
		g.fireV2AutomationRule(ctx, store, rule, message)
	}
}

func (g *Gateway) fireV2AutomationRule(ctx context.Context, store V2AutomationStore, rule registry.V2AutomationRule, message Message) {
	if rule.ConditionJSON != "" {
		matched, err := automationrule.Evaluate(json.RawMessage(rule.ConditionJSON), message.Payload)
		if err != nil {
			g.v2Failure(ctx, "rule_condition", err)
			return
		}
		if !matched {
			return
		}
	}
	count, err := store.CountV2PublishedAutomationExecutionsSince(ctx, rule.ID, time.Now().UTC().Add(-ruleFiringWindow))
	if err != nil {
		g.v2Failure(ctx, "rule_rate_limit", err)
		return
	}
	if count >= maxRuleFiringsPerRule {
		return
	}
	g.configMu.RLock()
	target, active := g.v2Target(rule.TargetDeviceID)
	g.configMu.RUnlock()
	if !active {
		g.v2Failure(ctx, "action_target", fmt.Errorf("target %q is not active", rule.TargetDeviceID))
		return
	}
	command, ok := target.Command(rule.CommandType)
	if !ok {
		g.v2Failure(ctx, "action_command", fmt.Errorf("target %q does not declare command %q", rule.TargetDeviceID, rule.CommandType))
		return
	}
	parameters, err := devicev2.ValidateFields(command.Parameters, []byte(rule.ParametersJSON))
	if err != nil {
		g.v2Failure(ctx, "action_parameters", err)
		return
	}
	commandPayload, commandID, err := transformJSONCommand(rule.CommandType, parameters)
	if err != nil {
		g.v2Failure(ctx, "command_encode", err)
		return
	}
	reserved, err := store.ReserveV2AutomationExecution(ctx, rule.ID, message.MessageID, commandID, rule.TargetDeviceID)
	if err != nil {
		g.v2Failure(ctx, "rule_dedup", err)
		return
	}
	if !reserved {
		return
	}
	published := false
	defer func() {
		if err := store.CompleteV2AutomationExecution(ctx, rule.ID, message.MessageID, published); err != nil {
			g.v2Failure(ctx, "execution_audit", err)
		}
	}()
	topic := "devices/" + rule.TargetDeviceID + "/command"
	if err := g.client.Publish(ctx, topic, commandPayload, qosAtLeastOnce, false); err != nil {
		g.v2Failure(ctx, "command_publish", err)
		return
	}
	published = true
	g.recordEvent(ActivityEvent{Timestamp: time.Now().UTC(), DeviceID: message.DeviceID, Topic: topic, Outcome: "v2_rule_fired", Detail: rule.ID})
}

func (g *Gateway) v2Target(deviceID string) (devicev2.Manifest, bool) {
	target, ok := g.v2Devices[deviceID]
	return target, ok
}

func (g *Gateway) v2Failure(ctx context.Context, kind string, err error) {
	if logger, ok := g.logger.(AutomationLogger); ok {
		logger.AutomationRecordFailed(ctx, "v2_"+kind, err)
	}
}
