// Package dynsec provisions narrow MQTT identities through Mosquitto's
// Dynamic Security control topic. It never reads or writes the plugin's JSON
// persistence file; that file remains exclusively owned by the broker.
package dynsec

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ricardossiqueira/iot-gateway/internal/devicev2"
)

// Controller executes one serialized DynSec control request. Serialisation is
// important because the 2.0 control protocol response has no request id.
type Controller interface {
	Execute(context.Context, []Command) error
}

// Command is one object in Dynamic Security's JSON "commands" array.
type Command map[string]any

// Manager maps a device's canonical topics to the smallest possible DynSec
// role. Passwords are generated in memory, returned once to the caller, and
// are never persisted by this package.
type Manager struct {
	control Controller
}

func NewManager(control Controller) (*Manager, error) {
	if control == nil {
		return nil, errors.New("dynsec controller is required")
	}
	return &Manager{control: control}, nil
}

func (m *Manager) Provision(ctx context.Context, id string, suffixes []string) (string, error) {
	if err := validID(id); err != nil {
		return "", err
	}
	role, err := roleFor(id, suffixes)
	if err != nil {
		return "", err
	}
	password, err := newPassword()
	if err != nil {
		return "", err
	}
	roleName := "device-" + id
	if err := m.control.Execute(ctx, []Command{{
		"command": "createRole", "rolename": roleName, "acls": role,
	}}); err != nil {
		return "", fmt.Errorf("create role for %q: %w", id, err)
	}
	if err := m.control.Execute(ctx, []Command{{
		"command": "createClient", "username": id, "clientid": id, "password": password,
		"roles": []map[string]any{{"rolename": roleName, "priority": 1}},
	}}); err != nil {
		// A role without a client grants nothing. Best-effort cleanup avoids
		// leaving administrative clutter on ordinary failures; an interrupted
		// operation remains visible to the caller and is safe to inspect.
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.control.Execute(rollbackCtx, []Command{{"command": "deleteRole", "rolename": roleName}})
		return "", fmt.Errorf("create client %q: %w", id, err)
	}
	return password, nil
}

// ProvisionV2 derives the only allowed MQTT grants directly from the v2
// manifest. Callers must not supply topic suffixes beside this method.
func (m *Manager) ProvisionV2(ctx context.Context, id string, manifest devicev2.Manifest) (string, error) {
	publish, subscribe := manifest.Channels()
	return m.Provision(ctx, id, append(publish, subscribe...))
}

func (m *Manager) Revoke(ctx context.Context, id string) error {
	if err := validID(id); err != nil {
		return err
	}
	if err := m.control.Execute(ctx, []Command{{"command": "deleteClient", "username": id}}); err != nil {
		return fmt.Errorf("delete client %q: %w", id, err)
	}
	// The role only belongs to this identity, so it can be removed too. A
	// successful deleteClient is the security boundary; failure here is not.
	if err := m.control.Execute(ctx, []Command{{"command": "deleteRole", "rolename": "device-" + id}}); err != nil {
		return fmt.Errorf("delete role for %q after revoking its client: %w", id, err)
	}
	return nil
}

func (m *Manager) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if err := validID(id); err != nil {
		return err
	}
	command := "disableClient"
	if enabled {
		command = "enableClient"
	}
	if err := m.control.Execute(ctx, []Command{{"command": command, "username": id}}); err != nil {
		return fmt.Errorf("%s %q: %w", command, id, err)
	}
	return nil
}

func roleFor(id string, suffixes []string) ([]map[string]any, error) {
	if len(suffixes) == 0 {
		return nil, errors.New("at least one topic is required")
	}
	acls := make([]map[string]any, 0, len(suffixes)*2)
	seen := make(map[string]bool, len(suffixes))
	for _, suffix := range suffixes {
		if seen[suffix] {
			continue
		}
		seen[suffix] = true
		topic := "devices/" + id + "/" + suffix
		switch suffix {
		case "telemetry", "state", "event", "command-result":
			acls = append(acls, map[string]any{"acltype": "publishClientSend", "topic": topic, "allow": true, "priority": 1})
		case "command":
			// Both grants are intentional: subscriptions are denied by default,
			// and the receive ACL remains correct if the broker uses the strict
			// receive default recommended by our deployment unit.
			acls = append(acls,
				map[string]any{"acltype": "subscribeLiteral", "topic": topic, "allow": true, "priority": 1},
				map[string]any{"acltype": "publishClientReceive", "topic": topic, "allow": true, "priority": 1},
			)
		default:
			return nil, fmt.Errorf("unknown topic suffix %q", suffix)
		}
	}
	return acls, nil
}

func newPassword() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate device password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func validID(id string) error {
	if id == "" || strings.ContainsAny(id, "/+#! \t\r\n") {
		return fmt.Errorf("invalid device id %q", id)
	}
	return nil
}
