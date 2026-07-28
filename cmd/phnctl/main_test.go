package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"phnctl/internal/auth"
)

func TestLoadConfigRejectsUnsafeHTTPBind(t *testing.T) {
	passwordHash, err := auth.HashPassword("configuration test password")
	if err != nil {
		t.Fatal(err)
	}
	secret := base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	t.Setenv("PHNCTL_USERNAME", "admin")
	t.Setenv("PHNCTL_PASSWORD_HASH", passwordHash)
	t.Setenv("PHNCTL_SESSION_SECRET", secret)
	t.Setenv("PHNCTL_TLS_CERT", "")
	t.Setenv("PHNCTL_TLS_KEY", "")
	t.Setenv("PHNCTL_INSECURE_HTTP", "true")
	t.Setenv("PHNCTL_BIND", "0.0.0.0:8443")
	t.Setenv("PHNCTL_ALLOW_ALL_INTERFACES", "true")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("loadConfig() error = %v, want loopback rejection", err)
	}

	t.Setenv("PHNCTL_BIND", "127.0.0.1:8443")
	if _, err := loadConfig(); err != nil {
		t.Fatalf("loopback development configuration rejected: %v", err)
	}
}

func TestLoadConfigRequiresTLSOnLAN(t *testing.T) {
	passwordHash, err := auth.HashPassword("configuration test password")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHNCTL_PASSWORD_HASH", passwordHash)
	t.Setenv("PHNCTL_SESSION_SECRET",
		base64.RawStdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	t.Setenv("PHNCTL_BIND", "192.168.1.239:8443")
	t.Setenv("PHNCTL_INSECURE_HTTP", "false")
	t.Setenv("PHNCTL_TLS_CERT", "")
	t.Setenv("PHNCTL_TLS_KEY", "")

	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("loadConfig() error = %v, want TLS requirement", err)
	}
}
