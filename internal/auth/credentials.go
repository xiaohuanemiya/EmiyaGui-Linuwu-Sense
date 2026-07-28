package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MinPasswordLength is the shortest password the first-run setup accepts. The
// panel is reachable over the LAN and can write hardware registers, so a
// throwaway password is not appropriate even on a home network.
const MinPasswordLength = 12

// Credentials is the persisted admin login. It lives in its own file rather
// than in the systemd EnvironmentFile so the server can rewrite it at runtime
// without having to parse and regenerate the session secret and TLS paths
// alongside it.
type Credentials struct {
	Username     string `json:"username"`
	PasswordHash string `json:"passwordHash"`
}

// ValidatePassword enforces the first-run password policy. It deliberately
// checks only length and obvious self-references: complexity rules push people
// towards predictable substitutions without adding real entropy.
func ValidatePassword(username, password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(password) > 1024 {
		return fmt.Errorf("password is too long")
	}
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("password must not be blank")
	}
	if username != "" && strings.EqualFold(strings.TrimSpace(password), strings.TrimSpace(username)) {
		return fmt.Errorf("password must not be the same as the username")
	}
	return nil
}

// LoadCredentials reads persisted credentials. A missing file is reported as
// os.ErrNotExist so callers can treat it as "not set up yet".
func LoadCredentials(path string) (Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var creds Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return Credentials{}, fmt.Errorf("%s is not valid credentials JSON: %w", path, err)
	}
	if creds.Username == "" {
		return Credentials{}, fmt.Errorf("%s has no username", path)
	}
	if err := ValidatePasswordHash(creds.PasswordHash); err != nil {
		return Credentials{}, fmt.Errorf("%s: %w", path, err)
	}
	return creds, nil
}

// SaveCredentials writes credentials atomically at 0600. The temporary file is
// created in the destination directory so the rename cannot cross filesystems
// and leave a partially written credentials file behind.
func SaveCredentials(path string, creds Credentials) error {
	if creds.Username == "" {
		return fmt.Errorf("username must not be empty")
	}
	if err := ValidatePasswordHash(creds.PasswordHash); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
