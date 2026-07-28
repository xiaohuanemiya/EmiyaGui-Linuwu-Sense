package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePasswordHash(encoded); err != nil {
		t.Fatalf("generated hash failed validation: %v", err)
	}
	if !VerifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword(encoded, "incorrect horse battery staple") {
		t.Fatal("incorrect password was accepted")
	}
	if VerifyPassword(strings.Replace(encoded, "210000", "10", 1), "correct horse battery staple") {
		t.Fatal("weak iteration count was accepted")
	}
}

func TestSessions(t *testing.T) {
	secret, _ := base64.RawStdEncoding.DecodeString("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	sessions, err := NewSessions(secret, "admin", 12*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	token, expires, err := sessions.New(now)
	if err != nil {
		t.Fatal(err)
	}
	if username, err := sessions.Validate(token, now.Add(time.Hour)); err != nil || username != "admin" {
		t.Fatalf("valid token rejected: username=%q err=%v", username, err)
	}
	if _, err := sessions.Validate(token+"tampered", now.Add(time.Hour)); err == nil {
		t.Fatal("tampered token accepted")
	}
	if _, err := sessions.Validate(token, expires); err == nil {
		t.Fatal("expired token accepted")
	}
}
