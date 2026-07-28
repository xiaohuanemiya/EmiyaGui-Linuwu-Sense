#!/usr/bin/env sh
set -eu

service_source=${1:-}
bind_address=${PHNCTL_INSTALL_BIND:-192.168.1.239:8443}
binary_path=${PHNCTL_INSTALL_BINARY:-"$HOME/.local/bin/phnctl"}
config_dir="$HOME/.config/phnctl"
environment_file="$config_dir/phnctl.env"
tls_dir="$config_dir/tls"
service_dir="$HOME/.config/systemd/user"
service_target="$service_dir/phnctl.service"
setup_token_file="$config_dir/setup-token.txt"

if [ -z "$service_source" ] || [ ! -f "$service_source" ]; then
  echo "Usage: install-user.sh /path/to/phnctl-user.service" >&2
  exit 2
fi
if [ ! -x "$binary_path" ]; then
  echo "The phnctl binary is missing or not executable: $binary_path" >&2
  exit 2
fi
if ! id -nG | tr ' ' '\n' | grep -qx linuwu_sense; then
  echo "The current login session is not a member of linuwu_sense." >&2
  exit 2
fi
if [ -e "$environment_file" ]; then
  echo "Refusing to overwrite existing configuration: $environment_file" >&2
  exit 3
fi

umask 077
install -d -m 0700 "$config_dir" "$tls_dir" "$service_dir"

session_secret=$(openssl rand -base64 32 | tr -d '\n')

openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 825 \
  -keyout "$tls_dir/key.pem" \
  -out "$tls_dir/cert.pem" \
  -subj "/CN=emiya-ubuntu" \
  -addext "subjectAltName=DNS:emiya-ubuntu,IP:192.168.1.239" \
  >/dev/null 2>&1
chmod 0600 "$tls_dir/key.pem"
chmod 0644 "$tls_dir/cert.pem"

# No PHNCTL_PASSWORD_HASH: the panel asks for a password on first open and
# persists it to PHNCTL_CREDENTIALS_FILE. Setting the hash here would skip that
# and pin whatever this script invented.
{
  printf 'PHNCTL_BIND=%s\n' "$bind_address"
  printf 'PHNCTL_USERNAME=admin\n'
  printf 'PHNCTL_CREDENTIALS_FILE=%s\n' "$config_dir/credentials"
  printf 'PHNCTL_SESSION_SECRET=%s\n' "$session_secret"
  printf 'PHNCTL_TLS_CERT=%s\n' "$tls_dir/cert.pem"
  printf 'PHNCTL_TLS_KEY=%s\n' "$tls_dir/key.pem"
  printf 'PHNCTL_SYSFS_ROOT=/\n'
  printf 'PHNCTL_TELEMETRY_SECONDS=2\n'
} > "$environment_file"
chmod 0600 "$environment_file"

install -m 0600 "$service_source" "$service_target"

systemctl --user daemon-reload
systemctl --user enable --now phnctl.service

attempt=0
while [ "$attempt" -lt 15 ]; do
  if curl --insecure --fail --silent --show-error "https://$bind_address/" >/dev/null 2>&1; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ "$attempt" -ge 15 ]; then
  echo "The service did not become reachable within 15 seconds." >&2
  exit 1
fi

if ! curl --insecure --fail --silent --show-error "https://$bind_address/api/setup" \
  | grep -q '"setupRequired":true'; then
  echo "The service is up but did not report that setup is pending." >&2
  echo "If a credentials file already exists, log in with the existing password." >&2
  exit 1
fi

setup_token=""
if [ -r "$setup_token_file" ]; then
  setup_token=$(cat "$setup_token_file")
fi

echo
echo "Predator Control is running at https://$bind_address"
echo
echo "Open it in a browser and set your own admin password. It will ask for the"
echo "one-time setup token below, which exists only until a password is set:"
echo
echo "    $setup_token"
echo
echo "The token is also in $setup_token_file and in"
echo "'journalctl --user -u phnctl'."

