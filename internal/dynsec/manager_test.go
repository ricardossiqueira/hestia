package dynsec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeController struct {
	calls [][]Command
	errAt map[int]error
}

func (f *fakeController) Execute(_ context.Context, commands []Command) error {
	f.calls = append(f.calls, commands)
	return f.errAt[len(f.calls)]
}

func TestProvisionCreatesNarrowRoleAndClient(t *testing.T) {
	controller := &fakeController{}
	manager, err := NewManager(controller)
	if err != nil {
		t.Fatal(err)
	}
	password, err := manager.Provision(context.Background(), "led-2", []string{"state", "command"})
	if err != nil {
		t.Fatal(err)
	}
	if len(password) < 40 || strings.ContainsAny(password, "+/=") {
		t.Fatalf("password is not an URL-safe random secret: %q", password)
	}
	if len(controller.calls) != 2 {
		t.Fatalf("calls = %#v", controller.calls)
	}
	role := controller.calls[0][0]
	if role["command"] != "createRole" || role["rolename"] != "device-led-2" {
		t.Fatalf("role = %#v", role)
	}
	acls := role["acls"].([]map[string]any)
	if len(acls) != 3 {
		t.Fatalf("ACLs = %#v", acls)
	}
	if acls[0]["acltype"] != "publishClientSend" || acls[0]["topic"] != "devices/led-2/state" {
		t.Fatalf("state ACL = %#v", acls[0])
	}
	if acls[1]["acltype"] != "subscribeLiteral" || acls[2]["acltype"] != "publishClientReceive" {
		t.Fatalf("command ACLs = %#v", acls[1:])
	}
	client := controller.calls[1][0]
	if client["command"] != "createClient" || client["username"] != "led-2" || client["clientid"] != "led-2" {
		t.Fatalf("client = %#v", client)
	}
}

func TestProvisionCleansRoleWhenClientCreationFails(t *testing.T) {
	controller := &fakeController{errAt: map[int]error{2: errors.New("broker unavailable")}}
	manager, _ := NewManager(controller)
	if _, err := manager.Provision(context.Background(), "led-2", []string{"command"}); err == nil {
		t.Fatal("Provision() error = nil")
	}
	if len(controller.calls) != 3 || controller.calls[2][0]["command"] != "deleteRole" {
		t.Fatalf("calls = %#v", controller.calls)
	}
}

func TestSetEnabledAndRevoke(t *testing.T) {
	controller := &fakeController{}
	manager, _ := NewManager(controller)
	if err := manager.SetEnabled(context.Background(), "led-2", false); err != nil {
		t.Fatal(err)
	}
	if err := manager.SetEnabled(context.Background(), "led-2", true); err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(context.Background(), "led-2"); err != nil {
		t.Fatal(err)
	}
	if got := []string{controller.calls[0][0]["command"].(string), controller.calls[1][0]["command"].(string), controller.calls[2][0]["command"].(string), controller.calls[3][0]["command"].(string)}; strings.Join(got, ",") != "disableClient,enableClient,deleteClient,deleteRole" {
		t.Fatalf("commands = %v", got)
	}
}

func TestCheckResponse(t *testing.T) {
	commands := []Command{{"command": "createClient"}}
	if err := checkResponse([]byte(`{"responses":[{"command":"createClient","data":{}}]}`), commands); err != nil {
		t.Fatal(err)
	}
	if err := checkResponse([]byte(`{"responses":[{"command":"createClient","error":"Client already exists"}]}`), commands); err == nil {
		t.Fatal("checkResponse() error = nil")
	}
}
