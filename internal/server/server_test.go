package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
	"time"

	"phnctl/internal/auth"
	"phnctl/internal/sysfs"
)

type testFS struct {
	files fstest.MapFS
}

func (t *testFS) ReadFile(name string) ([]byte, error)       { return fs.ReadFile(t.files, name) }
func (t *testFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(t.files, name) }
func (t *testFS) Stat(name string) (fs.FileInfo, error)      { return fs.Stat(t.files, name) }
func (t *testFS) WriteFile(name string, data []byte) error {
	file, ok := t.files[name]
	if !ok {
		return fs.ErrNotExist
	}
	file.Data = append([]byte(nil), data...)
	t.files[name] = file
	return nil
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	file := func(value string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(value)} }
	files := &testFS{files: fstest.MapFS{
		"sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense/fan_speed": file("0,0"),
		"sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense/version":   file("test"),
		"sys/firmware/acpi/platform_profile":                                                  file("balanced"),
		"sys/firmware/acpi/platform_profile_choices":                                          file("low-power quiet balanced balanced-performance performance"),
		"sys/class/power_supply/ACAD/type":                                                    file("Mains"),
		"sys/class/power_supply/ACAD/online":                                                  file("1"),
	}}
	passwordHash, err := auth.HashPassword("very secure test password")
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := base64.RawStdEncoding.DecodeString("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	sessions, err := auth.NewSessions(secret, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{
		Username: "admin", PasswordHash: passwordHash, Sessions: sessions,
		WriteInterval: time.Millisecond,
	}, sysfs.NewController(files))
	if err != nil {
		t.Fatal(err)
	}
	return server, passwordHash
}

func TestAuthenticationAndProtectedState(t *testing.T) {
	server, _ := newTestServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", response.Code)
	}

	body, _ := json.Marshal(map[string]string{
		"username": "admin", "password": "very secure test password",
	})
	request = httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader(body))
	request.Host = "panel.test"
	request.Header.Set("Origin", "https://panel.test")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie is not hardened: %#v", cookies)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated state status = %d: %s", response.Code, response.Body.String())
	}
}

func TestCrossOriginMutationRejected(t *testing.T) {
	server, _ := newTestServer(t)
	token, _, err := server.config.Sessions.New(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"profile":"balanced"}`)
	request := httptest.NewRequest(http.MethodPut, "/api/profile", body)
	request.Host = "panel.test"
	request.Header.Set("Origin", "https://evil.test")
	request.Header.Set("X-Requested-With", "phnctl")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation status = %d, want 403", response.Code)
	}
}

// newSetupServer builds a server with no password configured, i.e. one that is
// waiting for first-run setup, and returns it with its credentials path.
func newSetupServer(t *testing.T) (*Server, string) {
	t.Helper()
	file := func(value string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(value)} }
	files := &testFS{files: fstest.MapFS{
		"sys/firmware/acpi/platform_profile":         file("balanced"),
		"sys/firmware/acpi/platform_profile_choices": file("quiet balanced performance"),
	}}
	secret, _ := base64.RawStdEncoding.DecodeString("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	sessions, err := auth.NewSessions(secret, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	server, err := New(Config{
		Username: "admin", CredentialsPath: credentialsPath, Sessions: sessions,
		WriteInterval: time.Millisecond,
	}, sysfs.NewController(files))
	if err != nil {
		t.Fatal(err)
	}
	return server, credentialsPath
}

func postSetup(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/setup", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://"+request.Host)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

func TestSetupRequiredUntilPasswordChosen(t *testing.T) {
	server, _ := newSetupServer(t)

	request := httptest.NewRequest(http.MethodGet, "/api/setup", nil)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var status struct {
		SetupRequired     bool `json:"setupRequired"`
		MinPasswordLength int  `json:"minPasswordLength"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.SetupRequired {
		t.Fatal("a server with no password must report setupRequired")
	}
	if status.MinPasswordLength != auth.MinPasswordLength {
		t.Fatalf("minPasswordLength: got %d, want %d", status.MinPasswordLength, auth.MinPasswordLength)
	}

	// Protected routes must stay shut, and must say why.
	protected := httptest.NewRequest(http.MethodGet, "/api/state", nil)
	protectedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(protectedResponse, protected)
	if protectedResponse.Code != http.StatusForbidden {
		t.Fatalf("/api/state before setup: got %d, want 403", protectedResponse.Code)
	}
	if !bytes.Contains(protectedResponse.Body.Bytes(), []byte("setup_required")) {
		t.Fatalf("expected a setup_required code, got %s", protectedResponse.Body)
	}
}

func TestSetupRejectsWrongTokenAndWeakPassword(t *testing.T) {
	server, credentialsPath := newSetupServer(t)

	wrong := postSetup(t, server, `{"token":"not-the-token","username":"admin","password":"a strong enough password"}`)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", wrong.Code)
	}
	if _, err := os.Stat(credentialsPath); !os.IsNotExist(err) {
		t.Fatal("a rejected setup must not write credentials")
	}

	weak := postSetup(t, server, `{"token":"`+server.SetupToken()+`","username":"admin","password":"short"}`)
	if weak.Code != http.StatusBadRequest {
		t.Fatalf("weak password: got %d, want 400", weak.Code)
	}
	if !server.setupRequired() {
		t.Fatal("a rejected setup must leave the server in setup mode")
	}
}

func TestSetupStoresPasswordAndIsSingleUse(t *testing.T) {
	server, credentialsPath := newSetupServer(t)
	token := server.SetupToken()
	if token == "" {
		t.Fatal("a server awaiting setup must expose a token")
	}

	ok := postSetup(t, server, `{"token":"`+token+`","username":"emiya","password":"a properly long password"}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("setup: got %d (%s), want 200", ok.Code, ok.Body)
	}
	if ok.Result().Cookies() == nil || len(ok.Result().Cookies()) == 0 {
		t.Fatal("a successful setup should log the operator straight in")
	}
	if server.setupRequired() {
		t.Fatal("setup must clear setup mode")
	}
	if server.SetupToken() != "" {
		t.Fatal("the setup token must be discarded once used")
	}

	// Persisted so a restart keeps the chosen password.
	stored, err := auth.LoadCredentials(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Username != "emiya" {
		t.Fatalf("stored username: got %q, want %q", stored.Username, "emiya")
	}
	if !auth.VerifyPassword(stored.PasswordHash, "a properly long password") {
		t.Fatal("stored hash does not verify against the chosen password")
	}
	info, err := os.Stat(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode: got %o, want 600", info.Mode().Perm())
	}

	// Replaying the token must not re-open setup.
	replay := postSetup(t, server, `{"token":"`+token+`","username":"attacker","password":"another long password"}`)
	if replay.Code != http.StatusConflict {
		t.Fatalf("token replay: got %d, want 409", replay.Code)
	}

	// The chosen password now works for a normal login.
	login := httptest.NewRequest(http.MethodPost, "/api/login",
		bytes.NewBufferString(`{"username":"emiya","password":"a properly long password"}`))
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "http://"+login.Host)
	loginResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login after setup: got %d (%s), want 200", loginResponse.Code, loginResponse.Body)
	}
}

func TestConfiguredServerHasNoSetupToken(t *testing.T) {
	server, _ := newTestServer(t)
	if server.SetupToken() != "" {
		t.Fatal("a server with a configured password must not mint a setup token")
	}
	if server.setupRequired() {
		t.Fatal("a server with a configured password must not require setup")
	}
	response := postSetup(t, server, `{"token":"anything","username":"x","password":"a properly long password"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("setup on a configured server: got %d, want 409", response.Code)
	}
}
