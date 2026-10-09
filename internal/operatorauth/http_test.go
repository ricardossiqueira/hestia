package operatorauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegistrationLoginSessionAndLogout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Config{AdminUsername: "gateway-admin", AdminPassword: "admin-secret", SecureCookie: true})
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	server := httptest.NewServer(service.Handler(protected))
	defer server.Close()
	call := func(method, route, body string, cookie *http.Cookie, csrf string, admin bool) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+route, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		if admin {
			req.SetBasicAuth("gateway-admin", "admin-secret")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	status := call(http.MethodGet, "/auth/session", "", nil, "", false)
	var initial map[string]any
	if err := json.NewDecoder(status.Body).Decode(&initial); err != nil {
		t.Fatal(err)
	}
	if initial["authenticated"] != false || initial["needsRegistration"] != true {
		t.Fatalf("initial session=%v", initial)
	}
	registration := `{"username":"operator","password":"long-operator-password"}`
	if got := call(http.MethodPost, "/auth/register", registration, nil, "", false); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("registration without admin status=%d", got.StatusCode)
	}
	if got := call(http.MethodPost, "/auth/register", registration, nil, "", true); got.StatusCode != http.StatusCreated {
		t.Fatalf("registration status=%d", got.StatusCode)
	}
	if got := call(http.MethodPost, "/auth/register", registration, nil, "", true); got.StatusCode != http.StatusConflict {
		t.Fatalf("second registration status=%d", got.StatusCode)
	}
	if got := call(http.MethodPost, "/auth/login", `{"username":"operator","password":"wrong-password"}`, nil, "", false); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong login status=%d", got.StatusCode)
	}
	login := call(http.MethodPost, "/auth/login", `{"username":"operator","password":"long-operator-password"}`, nil, "", false)
	if login.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", login.StatusCode)
	}
	var loggedIn map[string]any
	if err := json.NewDecoder(login.Body).Decode(&loggedIn); err != nil {
		t.Fatal(err)
	}
	csrf, ok := loggedIn["csrfToken"].(string)
	if !ok || csrf == "" {
		t.Fatalf("login body=%v", loggedIn)
	}
	var cookie *http.Cookie
	for _, item := range login.Cookies() {
		if item.Name == cookieName {
			cookie = item
		}
	}
	if cookie == nil || !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie=%v", cookie)
	}
	if got := call(http.MethodPost, "/protected", `{}`, cookie, "", false); got.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", got.StatusCode)
	}
	if got := call(http.MethodPost, "/protected", `{}`, cookie, csrf, false); got.StatusCode != http.StatusOK {
		t.Fatalf("session API status=%d", got.StatusCode)
	}
	current := call(http.MethodGet, "/auth/session", "", cookie, "", false)
	var restored map[string]any
	if err := json.NewDecoder(current.Body).Decode(&restored); err != nil {
		t.Fatal(err)
	}
	if restored["username"] != "operator" || restored["csrfToken"] != csrf {
		t.Fatalf("restored session=%v", restored)
	}
	if got := call(http.MethodPost, "/auth/logout", "", cookie, csrf, false); got.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d", got.StatusCode)
	}
	if got := call(http.MethodPost, "/protected", `{}`, cookie, csrf, false); got.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d", got.StatusCode)
	}
}

func TestSessionSurvivesStoreRestartAndExpires(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterFirst(ctx, "operator", "long-operator-password"); err != nil {
		t.Fatal(err)
	}
	token, _, err := store.NewSession(ctx, "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, ok, err := store.Session(ctx, token); err != nil || !ok {
		t.Fatalf("restored session ok=%v err=%v", ok, err)
	}
	store.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, ok, err := store.Session(ctx, token); err != nil || ok {
		t.Fatalf("expired session ok=%v err=%v", ok, err)
	}
}

func TestSecureSessionRejectsPlainHTTPBrowserOrigin(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, Config{SecureCookie: true, AllowedOrigins: []string{"http://localhost:5173"}})
	request := httptest.NewRequest(http.MethodGet, "/auth/session", nil)
	request.Header.Set("Origin", "http://localhost:5173")
	response := httptest.NewRecorder()
	service.Handler(http.NotFoundHandler()).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("HTTP origin status=%d, want 403", response.Code)
	}
}
