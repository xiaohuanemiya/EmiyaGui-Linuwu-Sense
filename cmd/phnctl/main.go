package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"phnctl/internal/auth"
	"phnctl/internal/server"
	"phnctl/internal/sysfs"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("phnctl: %v", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	command := "serve"
	if len(arguments) > 0 {
		command = arguments[0]
	}
	switch command {
	case "serve":
		return serve()
	case "hash-password":
		return hashPassword()
	case "cert":
		return showCertificate(arguments[1:])
	case "version", "--version", "-version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (use serve, hash-password, cert, or version)", command)
	}
}

// showCertificate prints the identity and fingerprint of the TLS certificate so
// it can be checked by eye before being trusted in a browser. Installing it into
// a trust store is deliberately left to the operator.
func showCertificate(arguments []string) error {
	file := os.Getenv("PHNCTL_TLS_CERT")
	if len(arguments) > 0 {
		file = arguments[0]
	}
	if file == "" {
		return fmt.Errorf("no certificate given (pass a path or set PHNCTL_TLS_CERT)")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read certificate: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("%s does not contain a PEM certificate", file)
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}
	addresses := make([]string, 0, len(parsed.IPAddresses))
	for _, address := range parsed.IPAddresses {
		addresses = append(addresses, address.String())
	}
	sum := sha256.Sum256(parsed.Raw)
	fmt.Printf("path:         %s\n", file)
	fmt.Printf("subject:      %s\n", parsed.Subject)
	fmt.Printf("dns names:    %s\n", strings.Join(parsed.DNSNames, ", "))
	fmt.Printf("ip addresses: %s\n", strings.Join(addresses, ", "))
	fmt.Printf("not before:   %s\n", parsed.NotBefore.Format(time.RFC3339))
	fmt.Printf("not after:    %s\n", parsed.NotAfter.Format(time.RFC3339))
	fmt.Printf("sha256:       %s\n", fingerprint(sum[:]))
	return nil
}

func fingerprint(sum []byte) string {
	parts := make([]string, len(sum))
	for i, value := range sum {
		parts[i] = fmt.Sprintf("%02X", value)
	}
	return strings.Join(parts, ":")
}

// tlsNoiseWindow throttles the handshake errors a browser emits while it still
// distrusts the self-signed certificate.
const tlsNoiseWindow = 5 * time.Minute

// tlsNoiseFilter keeps one "TLS handshake error" line per window and counts the
// rest, so a genuine TLS fault stays visible instead of being buried by a
// reconnecting browser.
type tlsNoiseFilter struct {
	out      io.Writer
	mu       sync.Mutex
	lastSeen time.Time
	dropped  int
}

func (f *tlsNoiseFilter) Write(payload []byte) (int, error) {
	if !bytes.Contains(payload, []byte("TLS handshake error")) {
		return f.out.Write(payload)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.lastSeen.IsZero() && time.Since(f.lastSeen) < tlsNoiseWindow {
		f.dropped++
		return len(payload), nil
	}
	line := string(bytes.TrimRight(payload, "\n"))
	if f.dropped > 0 {
		line = fmt.Sprintf("%s (%d similar suppressed)", line, f.dropped)
	}
	f.lastSeen = time.Now()
	f.dropped = 0
	if _, err := fmt.Fprintln(f.out, line); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func hashPassword() error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1025))
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if len(raw) > 1024 {
		return fmt.Errorf("password is too long")
	}
	encoded, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	fmt.Println(encoded)
	return nil
}

func serve() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	controller := sysfs.NewController(sysfs.OSFileSystem{Root: config.sysfsRoot})
	if _, err := controller.Snapshot(); err != nil {
		return fmt.Errorf("initial hardware probe failed: %w", err)
	}
	sessions, err := auth.NewSessions(config.sessionSecret, config.username, config.sessionLifetime)
	if err != nil {
		return err
	}
	app, err := server.New(server.Config{
		Username:        config.username,
		PasswordHash:    config.passwordHash,
		CredentialsPath: config.credentialsPath,
		Sessions:        sessions,
		SecureCookies:   !config.insecureHTTP,
		WriteInterval:   config.writeInterval,
		TelemetryEvery:  config.telemetryInterval,
	}, controller)
	if err != nil {
		return err
	}
	if token := app.SetupToken(); token != "" {
		if err := announceSetupToken(config, token); err != nil {
			return err
		}
	}

	httpServer := &http.Server{
		Addr:              config.bind,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(&tlsNoiseFilter{out: os.Stderr}, "", log.LstdFlags),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.RunTelemetry(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	scheme := "https"
	if config.insecureHTTP {
		scheme = "http"
	}
	log.Printf("phnctl %s listening on %s://%s", version, scheme, config.bind)
	if config.insecureHTTP {
		err = httpServer.ListenAndServe()
	} else {
		err = httpServer.ListenAndServeTLS(config.tlsCert, config.tlsKey)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type appConfig struct {
	bind              string
	username          string
	passwordHash      string
	credentialsPath   string
	sessionSecret     []byte
	sessionLifetime   time.Duration
	tlsCert           string
	tlsKey            string
	insecureHTTP      bool
	sysfsRoot         string
	writeInterval     time.Duration
	telemetryInterval time.Duration
}

func loadConfig() (appConfig, error) {
	config := appConfig{
		bind:              envOr("PHNCTL_BIND", "192.168.1.239:8443"),
		username:          envOr("PHNCTL_USERNAME", "admin"),
		passwordHash:      os.Getenv("PHNCTL_PASSWORD_HASH"),
		tlsCert:           os.Getenv("PHNCTL_TLS_CERT"),
		tlsKey:            os.Getenv("PHNCTL_TLS_KEY"),
		sysfsRoot:         envOr("PHNCTL_SYSFS_ROOT", "/"),
		sessionLifetime:   12 * time.Hour,
		writeInterval:     150 * time.Millisecond,
		telemetryInterval: 2 * time.Second,
	}
	var err error
	config.insecureHTTP, err = envBool("PHNCTL_INSECURE_HTTP", false)
	if err != nil {
		return appConfig{}, err
	}
	allowAll, err := envBool("PHNCTL_ALLOW_ALL_INTERFACES", false)
	if err != nil {
		return appConfig{}, err
	}
	host, _, err := net.SplitHostPort(config.bind)
	if err != nil {
		return appConfig{}, fmt.Errorf("PHNCTL_BIND must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if (host == "" || (ip != nil && ip.IsUnspecified())) && !allowAll {
		return appConfig{}, fmt.Errorf("refusing an all-interface bind unless PHNCTL_ALLOW_ALL_INTERFACES=true")
	}
	if config.insecureHTTP && (ip == nil || !ip.IsLoopback()) {
		return appConfig{}, fmt.Errorf("insecure HTTP is only allowed on a loopback address")
	}
	if !config.insecureHTTP && (config.tlsCert == "" || config.tlsKey == "") {
		return appConfig{}, fmt.Errorf("PHNCTL_TLS_CERT and PHNCTL_TLS_KEY are required")
	}
	config.credentialsPath = envOr("PHNCTL_CREDENTIALS_FILE", defaultCredentialsPath())
	if config.passwordHash != "" {
		// An explicit hash in the environment always wins, so existing
		// deployments keep working and never fall into setup mode.
		if err := auth.ValidatePasswordHash(config.passwordHash); err != nil {
			return appConfig{}, fmt.Errorf("PHNCTL_PASSWORD_HASH: %w", err)
		}
	} else if config.credentialsPath != "" {
		credentials, loadErr := auth.LoadCredentials(config.credentialsPath)
		switch {
		case loadErr == nil:
			config.username, config.passwordHash = credentials.Username, credentials.PasswordHash
		case errors.Is(loadErr, os.ErrNotExist):
			// Nothing configured yet: the server will run first-run setup.
		default:
			return appConfig{}, loadErr
		}
	}
	if config.passwordHash == "" && config.credentialsPath == "" {
		return appConfig{}, fmt.Errorf("set PHNCTL_PASSWORD_HASH or PHNCTL_CREDENTIALS_FILE so a password can be configured")
	}
	config.sessionSecret, err = decodeSecret(os.Getenv("PHNCTL_SESSION_SECRET"))
	if err != nil {
		return appConfig{}, err
	}
	if seconds := os.Getenv("PHNCTL_TELEMETRY_SECONDS"); seconds != "" {
		value, parseErr := strconv.Atoi(seconds)
		if parseErr != nil || value < 1 || value > 30 {
			return appConfig{}, fmt.Errorf("PHNCTL_TELEMETRY_SECONDS must be between 1 and 30")
		}
		config.telemetryInterval = time.Duration(value) * time.Second
	}
	return config, nil
}

// defaultCredentialsPath keeps the password next to the rest of the per-user
// configuration. An empty result means we could not determine a home directory,
// in which case the operator has to set PHNCTL_CREDENTIALS_FILE explicitly.
func defaultCredentialsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "phnctl", "credentials")
}

// announceSetupToken puts the one-time token where the operator can find it:
// the service log, and a 0600 file next to the credentials. The file is removed
// by the setup handler's success path on the next start, since a token only
// exists while no password is configured.
func announceSetupToken(config appConfig, token string) error {
	log.Printf("first-run setup required -- no password is configured yet")
	log.Printf("setup token: %s", token)

	if config.credentialsPath == "" {
		return nil
	}
	path := filepath.Join(filepath.Dir(config.credentialsPath), "setup-token.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return fmt.Errorf("write setup token: %w", err)
	}
	log.Printf("setup token also written to %s", path)
	return nil
}

func decodeSecret(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, fmt.Errorf("PHNCTL_SESSION_SECRET is required")
	}
	secret, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		secret, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(secret) < 32 {
		return nil, fmt.Errorf("PHNCTL_SESSION_SECRET must be base64 for at least 32 random bytes")
	}
	return secret, nil
}

func envBool(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
