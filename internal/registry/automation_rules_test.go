package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/config"
)

func eventDevice(id string, enabled bool) config.Device {
	e := enabled
	return config.Device{ID: id, Type: "esp32", Enabled: &e, Topics: config.Topics{Event: "devices/" + id + "/event"}}
}

func commandDevice(id string, enabled bool) config.Device {
	e := enabled
	return config.Device{ID: id, Type: "esp32", Enabled: &e, Topics: config.Topics{Command: "devices/" + id + "/command"}}
}

// manifestWithCapabilities builds a manifest document (unlike the shared
// manifestDocument helper, which always declares empty capabilities) that
// declares exactly one event type and one command type, for tests that
// need CreateAutomationRule's manifest-aware validation to actually engage.
func manifestWithCapabilities(id, eventType, commandType string) string {
	return `{"schema_version":1,"id":"` + id + `","display_name":"` + id + `",` +
		`"provisioning":{"protocol":"http-nvs-v1","model":"` + id + `","required_protocol_version":1},` +
		`"mqtt":{"topics":["event","command"]},` +
		`"capabilities":{"commands":[{"type":"` + commandType + `","parameters":{"on":{"type":"boolean","required":true}}}],` +
		`"events":[{"type":"` + eventType + `","payload":{"pressed":{"type":"boolean","required":true}}}]}}`
}

// bindManifest creates, publishes and binds a manifest declaring exactly
// one event type and one command type to deviceID, for tests exercising
// CreateAutomationRule's manifest-aware validation.
func bindManifest(t *testing.T, store *Store, deviceID, eventType, commandType string) {
	t.Helper()
	ctx := context.Background()
	manifest, err := store.CreateDeviceManifestDraft(ctx, manifestWithCapabilities(deviceID+"-manifest", eventType, commandType), "tester")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishDeviceManifest(ctx, manifest.ID, manifest.Revision, "tester"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindDeviceManifest(ctx, deviceID, manifest.ID, manifest.Revision); err != nil {
		t.Fatal(err)
	}
}

func baseRule() AutomationRule {
	return AutomationRule{
		ID: "led1-to-led2", Enabled: true,
		SourceDeviceID: "led-1", EventType: "button_pressed",
		ActionDeviceID: "led-2", ActionCommandType: "set_led", ActionParametersJSON: `{"on":true}`,
	}
}

func TestCreateAutomationRuleValidatesID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	rule.ID = "Not Valid!"
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for malformed id, want error")
	}
}

func TestCreateAutomationRuleRequiresEnabledSourceEventTopic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for unknown source device, want error")
	}

	if _, err := store.AddDevice(ctx, commandDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for source device without an event topic, want error")
	}

	if _, err := store.AddDevice(ctx, eventDevice("led-3", false), "op-3"); err != nil {
		t.Fatal(err)
	}
	disabledRule := rule
	disabledRule.SourceDeviceID = "led-3"
	if _, err := store.CreateAutomationRule(ctx, disabledRule); err == nil {
		t.Error("CreateAutomationRule() error = nil for disabled source device, want error")
	}
}

func TestCreateAutomationRuleRequiresEnabledActionCommandTopic(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for unknown action device, want error")
	}
}

func TestCreateAutomationRuleRejectsMalformedCondition(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	rule.ConditionJSON = `{"unsupported_operator": [1, 2]}`
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for malformed condition, want error")
	}
}

func TestCreateAutomationRuleAcceptsValidCondition(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	rule.ConditionJSON = `{"==": [{"var": "pressed"}, true]}`
	got, err := store.CreateAutomationRule(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	if got.ConditionJSON != rule.ConditionJSON {
		t.Errorf("ConditionJSON = %q, want %q", got.ConditionJSON, rule.ConditionJSON)
	}
}

func TestCreateAutomationRuleEventTypeMustBeDeclaredWhenManifestBound(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	bindManifest(t, store, "led-1", "button_pressed", "set_led")

	rule := baseRule()
	rule.EventType = "not_declared"
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for event type not declared by bound manifest, want error")
	}

	rule.EventType = "button_pressed"
	if _, err := store.CreateAutomationRule(ctx, rule); err != nil {
		t.Errorf("CreateAutomationRule() error = %v for declared event type, want nil", err)
	}
}

func TestCreateAutomationRuleEventTypeAnyWithoutManifest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	rule.EventType = "anything_goes"
	if _, err := store.CreateAutomationRule(ctx, rule); err != nil {
		t.Errorf("CreateAutomationRule() error = %v without a bound manifest, want nil", err)
	}
}

func TestCreateAutomationRuleValidatesActionParametersAgainstBoundManifest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	bindManifest(t, store, "led-2", "button_pressed", "set_led")

	rule := baseRule()
	rule.ActionParametersJSON = `{"on": "not-a-boolean"}`
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for parameters violating the bound manifest's schema, want error")
	}

	rule.ActionParametersJSON = `{"on": true}`
	got, err := store.CreateAutomationRule(ctx, rule)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ActionSchemaValidated {
		t.Error("ActionSchemaValidated = false, want true when the action device has a bound manifest")
	}
}

func TestCreateAutomationRuleActionSchemaValidatedFalseWithoutManifest(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	got, err := store.CreateAutomationRule(ctx, baseRule())
	if err != nil {
		t.Fatal(err)
	}
	if got.ActionSchemaValidated {
		t.Error("ActionSchemaValidated = true, want false without a bound manifest")
	}
}

func TestCreateAutomationRuleRejectsMalformedActionParameters(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	rule.ActionParametersJSON = `not json`
	if _, err := store.CreateAutomationRule(ctx, rule); err == nil {
		t.Error("CreateAutomationRule() error = nil for non-JSON-object action parameters, want error")
	}
}

func TestCreateAutomationRuleDuplicateIDIsRejected(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	if _, err := store.CreateAutomationRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAutomationRule(ctx, rule); !errors.Is(err, ErrAutomationRuleAlreadyExists) {
		t.Errorf("CreateAutomationRule() error = %v, want ErrAutomationRuleAlreadyExists", err)
	}
}

func TestGetAndRemoveAutomationRuleNotFound(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.GetAutomationRule(ctx, "missing"); !errors.Is(err, ErrAutomationRuleNotFound) {
		t.Errorf("GetAutomationRule() error = %v, want ErrAutomationRuleNotFound", err)
	}
	if err := store.RemoveAutomationRule(ctx, "missing"); !errors.Is(err, ErrAutomationRuleNotFound) {
		t.Errorf("RemoveAutomationRule() error = %v, want ErrAutomationRuleNotFound", err)
	}
	if _, err := store.SetAutomationRuleEnabled(ctx, "missing", false); !errors.Is(err, ErrAutomationRuleNotFound) {
		t.Errorf("SetAutomationRuleEnabled() error = %v, want ErrAutomationRuleNotFound", err)
	}
}

func TestListAutomationRulesOrderedByID(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	second := baseRule()
	second.ID = "zzz-rule"
	first := baseRule()
	first.ID = "aaa-rule"
	if _, err := store.CreateAutomationRule(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAutomationRule(ctx, first); err != nil {
		t.Fatal(err)
	}
	rules, err := store.ListAutomationRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].ID != "aaa-rule" || rules[1].ID != "zzz-rule" {
		t.Errorf("ListAutomationRules() = %#v, want [aaa-rule, zzz-rule]", rules)
	}
}

func TestSetAutomationRuleEnabledToggles(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	rule := baseRule()
	if _, err := store.CreateAutomationRule(ctx, rule); err != nil {
		t.Fatal(err)
	}
	disabled, err := store.SetAutomationRuleEnabled(ctx, rule.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled {
		t.Error("Enabled = true after SetAutomationRuleEnabled(false)")
	}
	enabled, err := store.SetAutomationRuleEnabled(ctx, rule.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled.Enabled {
		t.Error("Enabled = false after SetAutomationRuleEnabled(true)")
	}
}

func TestUpdateAutomationRuleReplacesFields(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.AddDevice(ctx, eventDevice("led-1", true), "op-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddDevice(ctx, commandDevice("led-2", true), "op-2"); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAutomationRule(ctx, baseRule())
	if err != nil {
		t.Fatal(err)
	}
	updated := created
	updated.EventType = "changed_event"
	updated.ActionParametersJSON = `{"on": false}`
	got, err := store.UpdateAutomationRule(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventType != "changed_event" || got.ActionParametersJSON != `{"on": false}` {
		t.Errorf("UpdateAutomationRule() = %#v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt changed on update: got %v, want %v", got.CreatedAt, created.CreatedAt)
	}

	if _, err := store.UpdateAutomationRule(ctx, AutomationRule{ID: "missing", SourceDeviceID: "led-1", EventType: "x", ActionDeviceID: "led-2", ActionCommandType: "set_led", ActionParametersJSON: `{}`}); !errors.Is(err, ErrAutomationRuleNotFound) {
		t.Errorf("UpdateAutomationRule() error = %v, want ErrAutomationRuleNotFound", err)
	}
}
