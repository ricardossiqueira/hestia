package operatorauth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const cookieName = "hestia_session"

type Config struct {
	AdminUsername  string
	AdminPassword  string
	AllowedOrigins []string
	SecureCookie   bool
}

// Service is the local identity boundary. The public gateway only needs its
// Handler; account storage and browser session details stay in this package.
type Service struct {
	store    *Store
	config   Config
	mu       sync.Mutex
	failures map[string][]time.Time
}

func New(store *Store, config Config) *Service {
	return &Service{store: store, config: config, failures: make(map[string][]time.Time)}
}

func (s *Service) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if origin := r.Header.Get("Origin"); origin != "" && !s.allowedOrigin(r, origin) {
			writeError(w, http.StatusForbidden, "origin is not allowed")
			return
		}
		switch r.URL.Path {
		case "/auth/session":
			if r.Method != http.MethodGet {
				methodNotAllowed(w)
				return
			}
			s.session(w, r)
		case "/auth/register":
			if r.Method != http.MethodPost {
				methodNotAllowed(w)
				return
			}
			s.register(w, r)
		case "/auth/login":
			if r.Method != http.MethodPost {
				methodNotAllowed(w)
				return
			}
			s.login(w, r)
		case "/auth/logout":
			if r.Method != http.MethodPost {
				methodNotAllowed(w)
				return
			}
			s.logout(w, r)
		default:
			s.authorize(w, r, next)
		}
	})
}

func (s *Service) allowedOrigin(r *http.Request, origin string) bool {
	if s.config.SecureCookie && !strings.HasPrefix(origin, "https://") {
		return false
	}
	for _, allowed := range s.config.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return origin == "https://"+r.Host || origin == "http://"+r.Host
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return false
	}
	var extra any
	return errors.Is(decoder.Decode(&extra), io.EOF)
}

func (s *Service) adminValid(r *http.Request) bool {
	username, password, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(username), []byte(s.config.AdminUsername)) == 1 && subtle.ConstantTimeCompare([]byte(password), []byte(s.config.AdminPassword)) == 1
}

func (s *Service) authorize(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") && s.adminValid(r) {
		next.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	token := sessionCookie(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	session, ok, err := s.store.Session(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session unavailable")
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "session expired")
		return
	}
	if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) != 1 {
		writeError(w, http.StatusForbidden, "invalid CSRF token")
		return
	}
	// The sandboxed runtime does not need either browser secret.
	forwarded := r.Clone(r.Context())
	forwarded.Header.Del("Cookie")
	forwarded.Header.Del("X-CSRF-Token")
	next.ServeHTTP(w, forwarded)
}

func (s *Service) session(w http.ResponseWriter, r *http.Request) {
	needsRegistration, err := s.store.NeedsRegistration(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "account unavailable")
		return
	}
	token := sessionCookie(r)
	if token != "" {
		session, ok, err := s.store.Session(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session unavailable")
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": session.Username, "csrfToken": session.CSRFToken, "expiresAt": session.ExpiresAt.Format(time.RFC3339)})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "needsRegistration": needsRegistration})
}

func (s *Service) register(w http.ResponseWriter, r *http.Request) {
	if !s.adminValid(r) {
		writeError(w, http.StatusUnauthorized, "admin credentials required")
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &request) {
		writeError(w, http.StatusBadRequest, "invalid registration request")
		return
	}
	err := s.store.RegisterFirst(r.Context(), request.Username, request.Password)
	if errors.Is(err, ErrAlreadyRegistered) {
		writeError(w, http.StatusConflict, "operator already registered")
		return
	}
	if err != nil {
		if errors.Is(err, ErrInvalidAccount) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "registration unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"username": request.Username})
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &request) {
		writeError(w, http.StatusBadRequest, "invalid login request")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := ip
	if s.tooManyFailures(key) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	if err := s.store.VerifyPassword(r.Context(), request.Username, request.Password); err != nil {
		if !errors.Is(err, ErrInvalidCredentials) {
			writeError(w, http.StatusInternalServerError, "login unavailable")
			return
		}
		s.recordFailure(key)
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	s.clearFailures(key)
	token, session, err := s.store.NewSession(r.Context(), request.Username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "login unavailable")
		return
	}
	s.setCookie(w, token, int(sessionLifetime.Seconds()))
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": session.Username, "csrfToken": session.CSRFToken, "expiresAt": session.ExpiresAt.Format(time.RFC3339)})
}

func (s *Service) logout(w http.ResponseWriter, r *http.Request) {
	token := sessionCookie(r)
	if token != "" {
		session, ok, err := s.store.Session(r.Context(), token)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session unavailable")
			return
		}
		if ok && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(session.CSRFToken)) != 1 {
			writeError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		if err := s.store.RevokeSession(r.Context(), token); err != nil {
			writeError(w, http.StatusInternalServerError, "logout unavailable")
			return
		}
	}
	s.setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func sessionCookie(r *http.Request) string {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Service) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: s.config.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (s *Service) tooManyFailures(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-15 * time.Minute)
	values := s.failures[key]
	kept := values[:0]
	for _, value := range values {
		if value.After(cutoff) {
			kept = append(kept, value)
		}
	}
	s.failures[key] = kept
	return len(kept) >= 5
}

func (s *Service) recordFailure(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures[key] = append(s.failures[key], time.Now())
}

func (s *Service) clearFailures(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failures, key)
}
