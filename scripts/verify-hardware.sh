#!/usr/bin/env sh
set -eu

if [ "${1:-}" != "--accept-hardware-writes" ]; then
  echo "Refusing to change live hardware without --accept-hardware-writes." >&2
  exit 2
fi

profile_path=/sys/firmware/acpi/platform_profile
fan_path=/sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense/fan_speed
choices_path=/sys/firmware/acpi/platform_profile_choices
hold_seconds=${PHNCTL_VERIFY_HOLD_SECONDS:-3}

original_profile=$(cat "$profile_path")
original_fans=$(cat "$fan_path")

restore() {
  printf '%s' "$original_fans" > "$fan_path"
  printf '%s' "$original_profile" > "$profile_path"
}
trap restore EXIT HUP INT TERM

find_acer_hwmon() {
  for directory in /sys/class/hwmon/hwmon*; do
    if [ -r "$directory/name" ] && [ "$(cat "$directory/name")" = "acer" ]; then
      printf '%s' "$directory"
      return 0
    fi
  done
  return 1
}

acer_hwmon=$(find_acer_hwmon || true)
choices=" $(cat "$choices_path") "

printf 'original profile=%s fans=%s\n' "$original_profile" "$original_fans"
for profile in quiet balanced balanced-performance performance; do
  case "$choices" in
    *" $profile "*) ;;
    *) continue ;;
  esac

  printf '%s' "$profile" > "$profile_path"
  printf '50,50' > "$fan_path"
  sleep "$hold_seconds"

  actual_profile=$(cat "$profile_path")
  actual_fans=$(cat "$fan_path")
  cpu_rpm=unavailable
  gpu_rpm=unavailable
  if [ -n "$acer_hwmon" ]; then
    [ -r "$acer_hwmon/fan1_input" ] && cpu_rpm=$(cat "$acer_hwmon/fan1_input")
    [ -r "$acer_hwmon/fan2_input" ] && gpu_rpm=$(cat "$acer_hwmon/fan2_input")
  fi
  printf 'requested=%s actual_profile=%s actual_fans=%s rpm=%s/%s\n' \
    "$profile" "$actual_profile" "$actual_fans" "$cpu_rpm" "$gpu_rpm"
done

