package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	hashAlgorithm  = "pbkdf2-sha256"
	hashIterations = 210_000
	saltBytes      = 16
	hashBytes      = 32
)

var (
	ErrInvalidHash  = errors.New("invalid password hash")
	ErrInvalidToken = errors.New("invalid session token")
)

func HashPassword(password string) (string, error) {
	if len(password) < 12 {
		return "", fmt.Errorf("password must contain at least 12 characters")
	}
	if len(password) > 1024 {
		return "", fmt.Errorf("password is too long")
	}
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	derived := pbkdf2SHA256([]byte(password), salt, hashIterations, hashBytes)
	return fmt.Sprintf("%s$%d$%s$%s",
		hashAlgorithm,
		hashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(derived),
	), nil
}

func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashAlgorithm {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 100_000 || iterations > 2_000_000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(expected) != hashBytes {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, iterations, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func ValidatePasswordHash(encoded string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != hashAlgorithm {
		return ErrInvalidHash
	}
	if !VerifyPassword(encoded, "__phnctl_hash_validation__") {
		iterations, iterErr := strconv.Atoi(parts[1])
		salt, saltErr := base64.RawStdEncoding.DecodeString(parts[2])
		digest, digestErr := base64.RawStdEncoding.DecodeString(parts[3])
		if iterErr != nil || iterations < 100_000 || iterations > 2_000_000 ||
			saltErr != nil || len(salt) < 16 || len(salt) > 64 ||
			digestErr != nil || len(digest) != hashBytes {
			return ErrInvalidHash
		}
	}
	return nil
}

type Sessions struct {
	secret   []byte
	username string
	lifetime time.Duration
}

func NewSessions(secret []byte, username string, lifetime time.Duration) (*Sessions, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("session secret must contain at least 32 random bytes")
	}
	if username == "" || strings.Contains(username, "|") {
		return nil, fmt.Errorf("invalid username")
	}
	if lifetime < time.Minute {
		return nil, fmt.Errorf("session lifetime is too short")
	}
	return &Sessions{
		secret: append([]byte(nil), secret...), username: username, lifetime: lifetime,
	}, nil
}

func (s *Sessions) New(now time.Time) (string, time.Time, error) {
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, err
	}
	expires := now.Add(s.lifetime).UTC()
	payload := fmt.Sprintf("%s|%d|%s",
		s.username, expires.Unix(), base64.RawURLEncoding.EncodeToString(nonce))
	encodedPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signature := s.sign(encodedPayload)
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

func (s *Sessions) Validate(token string, now time.Time) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || subtle.ConstantTimeCompare(signature, s.sign(parts[0])) != 1 {
		return "", ErrInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalidToken
	}
	payload := strings.Split(string(raw), "|")
	if len(payload) != 3 || payload[0] != s.username {
		return "", ErrInvalidToken
	}
	expires, err := strconv.ParseInt(payload[1], 10, 64)
	if err != nil || now.Unix() >= expires {
		return "", ErrInvalidToken
	}
	nonce, err := base64.RawURLEncoding.DecodeString(payload[2])
	if err != nil || len(nonce) != 18 {
		return "", ErrInvalidToken
	}
	return payload[0], nil
}

func (s *Sessions) sign(payload string) []byte {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func pbkdf2SHA256(password, salt []byte, iterations, keyLength int) []byte {
	hashLength := sha256.Size
	blocks := (keyLength + hashLength - 1) / hashLength
	result := make([]byte, 0, blocks*hashLength)
	var counter [4]byte
	for block := 1; block <= blocks; block++ {
		counter[0] = byte(block >> 24)
		counter[1] = byte(block >> 16)
		counter[2] = byte(block >> 8)
		counter[3] = byte(block)

		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(salt)
		_, _ = mac.Write(counter[:])
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		result = append(result, t...)
	}
	return result[:keyLength]
}
