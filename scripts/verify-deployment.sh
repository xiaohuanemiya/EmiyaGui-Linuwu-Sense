#!/usr/bin/env sh
set -eu

bind_address=${PHNCTL_VERIFY_BIND:-192.168.1.239:8443}
password_file=${PHNCTL_VERIFY_PASSWORD_FILE:-"$HOME/.config/phnctl/initial-password.txt"}

if [ ! -r "$password_file" ]; then
  echo "Initial password file is not readable: $password_file" >&2
  exit 2
fi

password=$(cat "$password_file")
cookie_file=$(mktemp)
cleanup() {
  rm -f "$cookie_file"
}
trap cleanup EXIT HUP INT TERM

login_payload=$(printf '{"username":"admin","password":"%s"}' "$password")
curl --insecure --fail --silent --show-error \
  -c "$cookie_file" \
  -H 'Content-Type: application/json' \
  -H "Origin: https://$bind_address" \
  --data-binary "$login_payload" \
  "https://$bind_address/api/login" >/dev/null
curl --insecure --fail --silent --show-error \
  -b "$cookie_file" \
  "https://$bind_address/api/state"
printf '\n'

