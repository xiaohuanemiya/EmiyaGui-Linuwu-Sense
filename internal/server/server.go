package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"phnctl/internal/auth"
	"phnctl/internal/sysfs"
	"phnctl/internal/ui"
)

const sessionCookie = "phnctl_session"

type Config struct {
	Username     string
	PasswordHash string
	// CredentialsPath is where a password chosen through first-run setup is
	// persisted. Empty disables setup mode, which is what a deployment that
	// supplies PHNCTL_PASSWORD_HASH directly wants.
	CredentialsPath string
	Sessions        *auth.Sessions
	SecureCookies   bool
	WriteInterval   time.Duration
	TelemetryEvery  time.Duration
	Logger          *log.Logger
}

type Server struct {
	config       Config
	controller   *sysfs.Controller
	handler      http.Handler
	hub          *hub
	writeLimiter writeLimiter
	loginLimiter *loginLimiter
	degradedMu   sync.Mutex
	lastDegraded string

	// Credentials are mutable because first-run setup writes them at runtime.
	credentialsMu sync.RWMutex
	username      string
	passwordHash  string
	setupToken    string
}

func New(config Config, controller *sysfs.Controller) (*Server, error) {
	if config.Username == "" || config.Sessions == nil {
		return nil, fmt.Errorf("authentication configuration is incomplete")
	}
	// No password yet means first-run setup. Gate it behind a token the server
	// generates and the operator has to read off the host, otherwise whoever
	// reaches the port first on the LAN could claim the panel.
	setupToken := ""
	if config.PasswordHash == "" {
		if config.CredentialsPath == "" {
			return nil, fmt.Errorf("a password hash or a credentials path is required")
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generate setup token: %w", err)
		}
		setupToken = base64.RawURLEncoding.EncodeToString(raw)
	}
	if config.WriteInterval <= 0 {
		config.WriteInterval = 150 * time.Millisecond
	}
	if config.TelemetryEvery <= 0 {
		config.TelemetryEvery = 2 * time.Second
	}
	if config.Logger == nil {
		config.Logger = log.Default()
	}
	server := &Server{
		config: config, controller: controller, hub: newHub(), loginLimiter: newLoginLimiter(),
		username: config.Username, passwordHash: config.PasswordHash, setupToken: setupToken,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/setup", server.handleSetup)
	mux.HandleFunc("/api/login", server.handleLogin)
	mux.HandleFunc("/api/logout", server.requireAuth(server.handleLogout))
	mux.HandleFunc("/api/state", server.requireAuth(server.handleState))
	mux.HandleFunc("/api/ws", server.requireAuth(server.handleWebSocket))
	mux.HandleFunc("/api/profile", server.requireAuth(server.handleProfile))
	mux.HandleFunc("/api/fans", server.requireAuth(server.handleFans))
	mux.HandleFunc("/api/settings/", server.requireAuth(server.handleSetting))
	mux.HandleFunc("/api/keyboard/per-zone", server.requireAuth(server.handlePerZone))
	mux.HandleFunc("/api/keyboard/effect", server.requireAuth(server.handleEffect))
	mux.HandleFunc("/", server.handleStatic())
	server.handler = server.securityHeaders(mux)
	return server, nil
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) RunTelemetry(ctx context.Context) {
	ticker := time.NewTicker(s.config.TelemetryEvery)
	defer ticker.Stop()
	s.broadcastState()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.broadcastState()
		}
	}
}

// SetupToken is the one-time token that first-run setup requires, or empty when
// a password is already configured. The caller is responsible for getting it in
// front of the operator.
func (s *Server) SetupToken() string {
	s.credentialsMu.RLock()
	defer s.credentialsMu.RUnlock()
	return s.setupToken
}

func (s *Server) setupRequired() bool {
	s.credentialsMu.RLock()
	defer s.credentialsMu.RUnlock()
	return s.passwordHash == ""
}

func (s *Server) credentials() (string, string) {
	s.credentialsMu.RLock()
	defer s.credentialsMu.RUnlock()
	return s.username, s.passwordHash
}

// handleSetup reports whether first-run setup is pending (GET) and performs it
// (POST). Both are unauthenticated by necessity — there is no password yet — so
// POST is gated by the setup token and shares the login rate limiter.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"setupRequired":     s.setupRequired(),
			"minPasswordLength": auth.MinPasswordLength,
		})
	case http.MethodPost:
		s.performSetup(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) performSetup(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "request origin is not allowed", "origin")
		return
	}
	address := clientIP(r)
	if !s.loginLimiter.allow(address, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "too many attempts; try again later", "rate_limited")
		return
	}
	var request struct {
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}

	s.credentialsMu.Lock()
	if s.passwordHash != "" {
		s.credentialsMu.Unlock()
		writeError(w, http.StatusConflict, "a password is already configured", "already_configured")
		return
	}
	expected := s.setupToken
	s.credentialsMu.Unlock()

	if subtle.ConstantTimeCompare([]byte(request.Token), []byte(expected)) != 1 {
		s.loginLimiter.failed(address, time.Now())
		time.Sleep(250 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid setup token", "invalid_token")
		return
	}
	username := strings.TrimSpace(request.Username)
	if username == "" {
		username = s.config.Username
	}
	if err := auth.ValidatePassword(username, request.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "weak_password")
		return
	}
	hash, err := auth.HashPassword(request.Password)
	if err != nil {
		s.config.Logger.Printf("hash password: %v", err)
		writeError(w, http.StatusInternalServerError, "could not store the password", "internal")
		return
	}
	credentials := auth.Credentials{Username: username, PasswordHash: hash}
	if err := auth.SaveCredentials(s.config.CredentialsPath, credentials); err != nil {
		s.config.Logger.Printf("save credentials: %v", err)
		writeError(w, http.StatusInternalServerError, "could not store the password", "internal")
		return
	}

	s.credentialsMu.Lock()
	// Re-check under the write lock: two concurrent setup requests with the
	// right token must not both succeed and race over the stored password.
	if s.passwordHash != "" {
		s.credentialsMu.Unlock()
		writeError(w, http.StatusConflict, "a password is already configured", "already_configured")
		return
	}
	s.username, s.passwordHash, s.setupToken = username, hash, ""
	s.credentialsMu.Unlock()

	s.loginLimiter.succeeded(address)
	s.config.Logger.Printf("first-run setup completed for user %q", username)
	s.issueSession(w, username)
}

// issueSession sets the session cookie and writes the standard auth response.
func (s *Server) issueSession(w http.ResponseWriter, username string) {
	token, expires, err := s.config.Sessions.New(time.Now())
	if err != nil {
		s.config.Logger.Printf("create session: %v", err)
		writeError(w, http.StatusInternalServerError, "could not create session", "internal")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": username})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "request origin is not allowed", "origin")
		return
	}
	address := clientIP(r)
	if !s.loginLimiter.allow(address, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "too many login attempts; try again later", "rate_limited")
		return
	}
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	username, passwordHash := s.credentials()
	if passwordHash == "" {
		writeError(w, http.StatusConflict, "first-run setup has not been completed", "setup_required")
		return
	}
	usernameMatch := subtle.ConstantTimeCompare([]byte(request.Username), []byte(username)) == 1
	passwordMatch := auth.VerifyPassword(passwordHash, request.Password)
	if !usernameMatch || !passwordMatch {
		s.loginLimiter.failed(address, time.Now())
		time.Sleep(250 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid username or password", "invalid_credentials")
		return
	}
	s.loginLimiter.succeeded(address)
	s.issueSession(w, username)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !validMutation(r) {
		writeError(w, http.StatusForbidden, "request origin is not allowed", "origin")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	state, err := s.controller.Snapshot()
	if err != nil {
		s.config.Logger.Printf("read state: %v", err)
		writeError(w, http.StatusServiceUnavailable, "hardware state is temporarily unavailable", "hardware")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	var request struct {
		Profile string `json:"profile"`
	}
	if !s.prepareMutation(w, r, &request) {
		return
	}
	state, err := s.controller.SetProfile(request.Profile)
	s.finishMutation(w, state, err)
}

func (s *Server) handleFans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	var request struct {
		Mode      string `json:"mode"`
		CPU       int    `json:"cpu"`
		GPU       int    `json:"gpu"`
		Confirmed bool   `json:"confirmed"`
	}
	if !s.prepareMutation(w, r, &request) {
		return
	}
	var setting sysfs.FanSetting
	switch request.Mode {
	case "auto":
		setting = sysfs.FanSetting{CPU: 0, GPU: 0}
	case "max":
		setting = sysfs.FanSetting{CPU: 100, GPU: 100}
	case "manual":
		setting = sysfs.FanSetting{CPU: request.CPU, GPU: request.GPU}
	default:
		writeError(w, http.StatusBadRequest, "fan mode must be auto, manual, or max", "validation")
		return
	}
	state, err := s.controller.SetFans(setting, request.Confirmed)
	s.finishMutation(w, state, err)
}

func (s *Server) handleSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/settings/")
	var request struct {
		Enabled   *bool `json:"enabled"`
		Value     *int  `json:"value"`
		Confirmed bool  `json:"confirmed"`
	}
	if !s.prepareMutation(w, r, &request) {
		return
	}
	if name == "usb-charging" {
		if request.Value == nil {
			writeError(w, http.StatusBadRequest, "value is required", "validation")
			return
		}
		state, err := s.controller.SetUSBCharging(*request.Value)
		s.finishMutation(w, state, err)
		return
	}
	if request.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required", "validation")
		return
	}
	state, err := s.controller.SetBoolean(name, *request.Enabled, request.Confirmed)
	s.finishMutation(w, state, err)
}

func (s *Server) handlePerZone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	var request sysfs.ZoneSetting
	if !s.prepareMutation(w, r, &request) {
		return
	}
	state, err := s.controller.SetPerZone(request)
	s.finishMutation(w, state, err)
}

func (s *Server) handleEffect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	var request sysfs.EffectSetting
	if !s.prepareMutation(w, r, &request) {
		return
	}
	state, err := s.controller.SetEffect(request)
	s.finishMutation(w, state, err)
}

func (s *Server) prepareMutation(w http.ResponseWriter, r *http.Request, target any) bool {
	if !validMutation(r) {
		writeError(w, http.StatusForbidden, "request origin is not allowed", "origin")
		return false
	}
	if err := decodeJSON(r, target); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return false
	}
	if retryAfter, ok := s.writeLimiter.allow(time.Now(), s.config.WriteInterval); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()+0.5))))
		writeError(w, http.StatusTooManyRequests, "hardware writes are rate-limited", "rate_limited")
		return false
	}
	return true
}

func (s *Server) finishMutation(w http.ResponseWriter, state sysfs.State, err error) {
	if err != nil {
		switch {
		case errors.Is(err, sysfs.ErrUnsupported):
			writeError(w, http.StatusNotFound, err.Error(), "unsupported")
		case errors.Is(err, sysfs.ErrConflict):
			writeError(w, http.StatusConflict, err.Error(), "conflict")
		case errors.Is(err, sysfs.ErrConfirmation):
			writeError(w, http.StatusConflict, err.Error(), "confirmation_required")
		default:
			writeError(w, http.StatusBadRequest, err.Error(), "validation")
		}
		return
	}
	writeJSON(w, http.StatusOK, state)
	payload, _ := json.Marshal(state)
	s.hub.broadcast(payload)
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "request origin is not allowed", "origin")
		return
	}
	client, err := s.hub.upgrade(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "websocket")
		return
	}
	if state, err := s.controller.Snapshot(); err == nil {
		payload, _ := json.Marshal(state)
		_ = client.writeFrame(0x1, payload)
	}
	go func() {
		defer s.hub.remove(client)
		_ = client.readLoop()
	}()
}

func (s *Server) broadcastState() {
	// Every attribute in a snapshot costs a WMI round trip to the EC. With no
	// browser attached there is nobody to send it to, so skip the poll entirely
	// rather than burn CPU and battery on a discarded reading.
	if !s.hub.hasClients() {
		return
	}
	state, err := s.controller.Snapshot()
	if err != nil {
		s.config.Logger.Printf("telemetry read: %v", err)
		return
	}
	s.logDegraded(state.Degraded)
	payload, err := json.Marshal(state)
	if err == nil {
		s.hub.broadcast(payload)
	}
}

// logDegraded reports failing sensors once per change instead of on every tick,
// so a persistently broken attribute does not bury everything else in the log.
func (s *Server) logDegraded(degraded []string) {
	signature := strings.Join(degraded, ",")
	s.degradedMu.Lock()
	changed := signature != s.lastDegraded
	s.lastDegraded = signature
	s.degradedMu.Unlock()
	if !changed {
		return
	}
	if signature == "" {
		s.config.Logger.Printf("all hardware sections readable again")
		return
	}
	s.config.Logger.Printf("degraded hardware sections: %s", signature)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// A session minted before setup would otherwise outlive it. Reject
		// everything until a password exists so the UI can show the setup form.
		if s.setupRequired() {
			writeError(w, http.StatusForbidden, "first-run setup has not been completed", "setup_required")
			return
		}
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required", "unauthorized")
			return
		}
		if _, err := s.config.Sessions.Validate(cookie.Value, time.Now()); err != nil {
			writeError(w, http.StatusUnauthorized, "authentication required", "unauthorized")
			return
		}
		next(w, r)
	}
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; "+
				"form-action 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self' ws: wss:")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.config.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleStatic() http.HandlerFunc {
	assets, err := fs.Sub(ui.Files, "dist")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(assets))
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			methodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		fileServer.ServeHTTP(w, r)
	}
}

func validMutation(r *http.Request) bool {
	return sameOrigin(r) && r.Header.Get("X-Requested-With") == "phnctl"
}

func sameOrigin(r *http.Request) bool {
	raw := r.Header.Get("Origin")
	if raw == "" {
		return true
	}
	origin, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(origin.Host, r.Host)
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed", "method")
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

type writeLimiter struct {
	mu   sync.Mutex
	last time.Time
}

func (w *writeLimiter) allow(now time.Time, interval time.Duration) (time.Duration, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if elapsed := now.Sub(w.last); !w.last.IsZero() && elapsed < interval {
		return interval - elapsed, false
	}
	w.last = now
	return 0, true
}

type loginRecord struct {
	failures []time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	records map[string]loginRecord
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{records: make(map[string]loginRecord)}
}

func (l *loginLimiter) allow(address string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	record := l.records[address]
	cutoff := now.Add(-15 * time.Minute)
	recent := record.failures[:0]
	for _, attempt := range record.failures {
		if attempt.After(cutoff) {
			recent = append(recent, attempt)
		}
	}
	record.failures = recent
	l.records[address] = record
	return len(recent) < 5
}

func (l *loginLimiter) failed(address string, now time.Time) {
	l.mu.Lock()
	record := l.records[address]
	record.failures = append(record.failures, now)
	l.records[address] = record
	l.mu.Unlock()
}

func (l *loginLimiter) succeeded(address string) {
	l.mu.Lock()
	delete(l.records, address)
	l.mu.Unlock()
}
