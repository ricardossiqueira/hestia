package apigateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ricardossiqueira/iot-gateway/internal/operatorauth"
)

func TestOperatorSessionProtectsPublicGateway(t *testing.T) {
	backend, recorded := newFakeInternalAPI(t)
	store, err := operatorauth.Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RegisterFirst(context.Background(), "operator", "long-operator-password"); err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Address: "127.0.0.1:0", InternalAPIURL: backend.URL,
		Credentials:  Credentials{Username: "admin", Password: "admin-secret"},
		OperatorAuth: operatorauth.New(store, operatorauth.Config{AdminUsername: "admin", AdminPassword: "admin-secret"}),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	public := httptest.NewServer(server.http.Handler)
	defer public.Close()
	login, err := http.Post(public.URL+"/auth/login", "application/json", strings.NewReader(`{"username":"operator","password":"long-operator-password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", login.StatusCode)
	}
	var session struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.NewDecoder(login.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	if session.CSRFToken == "" || len(login.Cookies()) == 0 {
		t.Fatal("login did not issue a session")
	}
	call := func(csrf string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, public.URL+"/iot.gateway.api.v1.GatewayService/GetQueueSummary", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(login.Cookies()[0])
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		return response.StatusCode
	}
	if got := call(""); got != http.StatusForbidden {
		t.Fatalf("without CSRF status=%d", got)
	}
	if got := call(session.CSRFToken); got != http.StatusOK {
		t.Fatalf("authenticated call status=%d", got)
	}
	if recorded.auth != "" {
		t.Fatalf("session secret reached internal API as authorization: %q", recorded.auth)
	}
	if recorded.cookie != "" {
		t.Fatalf("session cookie reached internal API: %q", recorded.cookie)
	}
}
