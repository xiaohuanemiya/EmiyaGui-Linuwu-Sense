package sysfs

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	predatorBase = "sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/predator_sense"
	keyboardBase = "sys/module/linuwu_sense/drivers/platform:acer-wmi/acer-wmi/four_zoned_kb"
	profilePath  = "sys/firmware/acpi/platform_profile"
	choicesPath  = "sys/firmware/acpi/platform_profile_choices"
)

var (
	ErrUnsupported  = errors.New("feature is not supported by this machine")
	ErrConflict     = errors.New("operation conflicts with the active profile")
	ErrConfirmation = errors.New("explicit confirmation is required")
)

type Capabilities struct {
	FanControl         bool `json:"fanControl"`
	FanTelemetry       bool `json:"fanTelemetry"`
	ThermalProfile     bool `json:"thermalProfile"`
	BatteryLimiter     bool `json:"batteryLimiter"`
	BatteryCalibration bool `json:"batteryCalibration"`
	BacklightTimeout   bool `json:"backlightTimeout"`
	LCDOverride        bool `json:"lcdOverride"`
	USBCharging        bool `json:"usbCharging"`
	BootAnimationSound bool `json:"bootAnimationSound"`
	KeyboardPerZone    bool `json:"keyboardPerZone"`
	KeyboardEffects    bool `json:"keyboardEffects"`
	CPUTemperature     bool `json:"cpuTemperature"`
	GPUTemperature     bool `json:"gpuTemperature"`
}

type ProfileOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type ProfileState struct {
	Value     string          `json:"value"`
	Label     string          `json:"label"`
	Available []ProfileOption `json:"available"`
}

type FanState struct {
	Mode          string `json:"mode"`
	CPU           int    `json:"cpu"`
	GPU           int    `json:"gpu"`
	CPURPM        *int   `json:"cpuRpm"`
	GPURPM        *int   `json:"gpuRpm"`
	ManualAllowed bool   `json:"manualAllowed"`
}

type TemperatureState struct {
	CPU *float64 `json:"cpu"`
	GPU *float64 `json:"gpu"`
}

type SettingsState struct {
	BatteryLimiter     bool `json:"batteryLimiter"`
	BatteryCalibration bool `json:"batteryCalibration"`
	BacklightTimeout   bool `json:"backlightTimeout"`
	LCDOverride        bool `json:"lcdOverride"`
	USBCharging        int  `json:"usbCharging"`
	BootAnimationSound bool `json:"bootAnimationSound"`
}

type KeyboardState struct {
	PerZone *ZoneSetting   `json:"perZone"`
	Effect  *EffectSetting `json:"effect"`
}

type State struct {
	Timestamp     time.Time        `json:"timestamp"`
	PowerSource   string           `json:"powerSource"`
	Profile       ProfileState     `json:"profile"`
	Fans          FanState         `json:"fans"`
	Temperatures  TemperatureState `json:"temperatures"`
	Settings      SettingsState    `json:"settings"`
	Keyboard      KeyboardState    `json:"keyboard"`
	Capabilities  Capabilities     `json:"capabilities"`
	DriverVersion string           `json:"driverVersion"`
	// Degraded names the sections whose reads failed on this pass. Everything
	// else in the snapshot is still valid.
	Degraded []string `json:"degraded"`
}

// defaultSlowTTL bounds how stale a cached setting may get. Every attribute
// under predator_sense costs a WMI round trip to the EC, and these only change
// when this service writes them, so re-reading them twice a second is pure
// wasted ACPI traffic.
const defaultSlowTTL = 30 * time.Second

// sensorPaths caches resolved hwmon inputs so a telemetry tick does not have to
// re-enumerate /sys/class/hwmon and re-read every name file. Only paths that
// existed at resolve time are stored, so an empty field means "no such sensor"
// rather than "not looked up yet".
type sensorPaths struct {
	cpuTemp string
	gpuTemp string
	cpuFan  string
	gpuFan  string
}

// slowState holds values that only change when this service writes them.
type slowState struct {
	stamp          time.Time
	settings       SettingsState
	keyboard       KeyboardState
	profileChoices []string
	driverVersion  string
	degraded       []string
}

type Controller struct {
	fs      FileSystem
	mu      sync.RWMutex
	caps    Capabilities
	sensors sensorPaths
	slow    slowState
	slowTTL time.Duration
}

func NewController(files FileSystem) *Controller {
	controller := &Controller{fs: files, slowTTL: defaultSlowTTL}
	controller.sensors = controller.resolveSensors()
	controller.caps = controller.probe()
	return controller
}

func (c *Controller) RefreshCapabilities() Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caps = c.probe()
	return c.caps
}

func (c *Controller) Capabilities() Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.caps
}

// Snapshot takes the write lock because a read refreshes the sensor-path and
// slow-value caches. EC access is serial anyway, so nothing is lost.
func (c *Controller) Snapshot() (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot()
}

// invalidateSlow forces the next snapshot to re-read the cached settings, so a
// write is always followed by a truthful read-back.
func (c *Controller) invalidateSlow() {
	c.slow.stamp = time.Time{}
}

func (c *Controller) probe() Capabilities {
	exists := func(file string) bool {
		_, err := c.fs.Stat(file)
		return err == nil
	}
	caps := Capabilities{
		FanControl:         exists(path.Join(predatorBase, "fan_speed")),
		ThermalProfile:     exists(profilePath) && exists(choicesPath),
		BatteryLimiter:     exists(path.Join(predatorBase, "battery_limiter")),
		BatteryCalibration: exists(path.Join(predatorBase, "battery_calibration")),
		BacklightTimeout:   exists(path.Join(predatorBase, "backlight_timeout")),
		LCDOverride:        exists(path.Join(predatorBase, "lcd_override")),
		USBCharging:        exists(path.Join(predatorBase, "usb_charging")),
		BootAnimationSound: exists(path.Join(predatorBase, "boot_animation_sound")),
		KeyboardPerZone:    exists(path.Join(keyboardBase, "per_zone_mode")),
		KeyboardEffects:    exists(path.Join(keyboardBase, "four_zone_mode")),
	}
	cpu, gpu := c.readTemperatures()
	caps.CPUTemperature = cpu != nil
	caps.GPUTemperature = gpu != nil
	cpuRPM, gpuRPM := c.readFanRPM()
	caps.FanTelemetry = cpuRPM != nil || gpuRPM != nil
	return caps
}

// snapshot reads the live hardware state. A failing attribute is recorded in
// State.Degraded and left at its zero value instead of aborting the whole read:
// one flaky ACPI attribute must not blank the entire UI.
//
// Only the fast-moving values are read every call. Settings, keyboard state and
// the profile choice list come from a cache refreshed at slowTTL, or
// immediately after a write.
func (c *Controller) snapshot() (State, error) {
	state := State{
		Timestamp:    time.Now().UTC(),
		PowerSource:  c.readPowerSource(),
		Capabilities: c.caps,
	}
	var degraded []string
	note := func(section string) { degraded = append(degraded, section) }

	if c.caps.ThermalProfile {
		current, err := c.readText(profilePath)
		if err != nil {
			note("thermalProfile")
		} else {
			state.Profile.Value = current
			state.Profile.Label = profileForValue(current).Label
		}
	}
	if c.caps.FanControl {
		raw, err := c.readText(path.Join(predatorBase, "fan_speed"))
		if err != nil {
			note("fanControl")
		} else if fans, err := ParseFanSetting(raw); err != nil {
			note("fanControl")
		} else {
			state.Fans.CPU, state.Fans.GPU = fans.CPU, fans.GPU
			switch {
			case fans.CPU == 0 && fans.GPU == 0:
				state.Fans.Mode = "auto"
			case fans.CPU == 100 && fans.GPU == 100:
				state.Fans.Mode = "max"
			default:
				state.Fans.Mode = "manual"
			}
			state.Fans.ManualAllowed = state.Profile.Value != "quiet"
		}
	}
	state.Fans.CPURPM, state.Fans.GPURPM = c.readFanRPM()
	state.Temperatures.CPU, state.Temperatures.GPU = c.readTemperatures()
	if c.caps.FanTelemetry && state.Fans.CPURPM == nil && state.Fans.GPURPM == nil {
		note("fanTelemetry")
	}
	if c.caps.CPUTemperature && state.Temperatures.CPU == nil {
		note("cpuTemperature")
	}
	if c.caps.GPUTemperature && state.Temperatures.GPU == nil {
		note("gpuTemperature")
	}

	if time.Since(c.slow.stamp) > c.slowTTL {
		c.refreshSlow()
	}
	state.Settings = c.slow.settings
	state.Keyboard = c.slow.keyboard
	state.DriverVersion = c.slow.driverVersion
	if c.caps.ThermalProfile {
		state.Profile.Available = availableProfiles(c.slow.profileChoices, state.PowerSource)
	}
	state.Degraded = append(degraded, c.slow.degraded...)
	return state, nil
}

// refreshSlow re-reads the attributes that only change on a write. Callers must
// hold the write lock.
func (c *Controller) refreshSlow() {
	slow := slowState{stamp: time.Now()}
	note := func(section string) { slow.degraded = append(slow.degraded, section) }

	if c.caps.ThermalProfile {
		if choices, err := c.readText(choicesPath); err != nil {
			note("profileChoices")
		} else {
			slow.profileChoices = strings.Fields(choices)
		}
	}
	readBool := func(enabled bool, attribute, section string, target *bool) {
		if !enabled {
			return
		}
		value, err := c.readBool(path.Join(predatorBase, attribute))
		if err != nil {
			note(section)
			return
		}
		*target = value
	}
	readBool(c.caps.BatteryLimiter, "battery_limiter", "batteryLimiter", &slow.settings.BatteryLimiter)
	readBool(c.caps.BatteryCalibration, "battery_calibration", "batteryCalibration", &slow.settings.BatteryCalibration)
	readBool(c.caps.BacklightTimeout, "backlight_timeout", "backlightTimeout", &slow.settings.BacklightTimeout)
	readBool(c.caps.LCDOverride, "lcd_override", "lcdOverride", &slow.settings.LCDOverride)
	readBool(c.caps.BootAnimationSound, "boot_animation_sound", "bootAnimationSound", &slow.settings.BootAnimationSound)

	if c.caps.USBCharging {
		raw, err := c.readText(path.Join(predatorBase, "usb_charging"))
		if err != nil {
			note("usbCharging")
		} else if value, err := strconv.Atoi(raw); err != nil ||
			(value != 0 && value != 10 && value != 20 && value != 30) {
			note("usbCharging")
		} else {
			slow.settings.USBCharging = value
		}
	}
	if c.caps.KeyboardPerZone {
		raw, err := c.readText(path.Join(keyboardBase, "per_zone_mode"))
		if err != nil {
			note("keyboardPerZone")
		} else if value, err := ParseZoneSetting(raw); err != nil {
			note("keyboardPerZone")
		} else {
			slow.keyboard.PerZone = &value
		}
	}
	if c.caps.KeyboardEffects {
		raw, err := c.readText(path.Join(keyboardBase, "four_zone_mode"))
		if err != nil {
			note("keyboardEffects")
		} else if value, err := ParseEffectSetting(raw); err != nil {
			note("keyboardEffects")
		} else {
			slow.keyboard.Effect = &value
		}
	}
	if raw, err := c.readText(path.Join(predatorBase, "version")); err == nil {
		slow.driverVersion = raw
	}
	c.slow = slow
}

func (c *Controller) SetProfile(value string) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.caps.ThermalProfile {
		return State{}, ErrUnsupported
	}
	choicesRaw, err := c.readText(choicesPath)
	if err != nil {
		return State{}, err
	}
	allowed := false
	for _, option := range availableProfiles(strings.Fields(choicesRaw), c.readPowerSource()) {
		if option.Value == value {
			allowed = true
			break
		}
	}
	if !allowed {
		return State{}, fmt.Errorf("profile %q is not available on the current power source", value)
	}
	if err := c.fs.WriteFile(profilePath, []byte(value)); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) SetFans(setting FanSetting, confirmed bool) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.caps.FanControl {
		return State{}, ErrUnsupported
	}
	if err := setting.Validate(); err != nil {
		return State{}, err
	}
	profile, _ := c.readText(profilePath)
	isAuto := setting.CPU == 0 && setting.GPU == 0
	if profile == "quiet" && !isAuto {
		return State{}, fmt.Errorf("%w: manual fan control is disabled in Silent mode", ErrConflict)
	}
	isHighPerformance := profile == "balanced-performance" || profile == "performance"
	if isHighPerformance && !isAuto && (setting.CPU < 20 || setting.GPU < 20) && !confirmed {
		return State{}, ErrConfirmation
	}
	if err := c.fs.WriteFile(path.Join(predatorBase, "fan_speed"), []byte(setting.String())); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) SetBoolean(name string, value, confirmed bool) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var supported bool
	var file string
	switch name {
	case "battery-limiter":
		supported, file = c.caps.BatteryLimiter, "battery_limiter"
	case "battery-calibration":
		supported, file = c.caps.BatteryCalibration, "battery_calibration"
		if value && !confirmed {
			return State{}, ErrConfirmation
		}
	case "backlight-timeout":
		supported, file = c.caps.BacklightTimeout, "backlight_timeout"
	case "lcd-override":
		supported, file = c.caps.LCDOverride, "lcd_override"
	case "boot-animation-sound":
		supported, file = c.caps.BootAnimationSound, "boot_animation_sound"
	default:
		return State{}, ErrUnsupported
	}
	if !supported {
		return State{}, ErrUnsupported
	}
	raw := "0"
	if value {
		raw = "1"
	}
	if err := c.fs.WriteFile(path.Join(predatorBase, file), []byte(raw)); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) SetUSBCharging(value int) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.caps.USBCharging {
		return State{}, ErrUnsupported
	}
	if value != 0 && value != 10 && value != 20 && value != 30 {
		return State{}, fmt.Errorf("USB charging must be one of 0, 10, 20, or 30")
	}
	if err := c.fs.WriteFile(path.Join(predatorBase, "usb_charging"), []byte(strconv.Itoa(value))); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) SetPerZone(value ZoneSetting) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.caps.KeyboardPerZone {
		return State{}, ErrUnsupported
	}
	if err := value.Validate(); err != nil {
		return State{}, err
	}
	if err := c.fs.WriteFile(path.Join(keyboardBase, "per_zone_mode"), []byte(value.String())); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) SetEffect(value EffectSetting) (State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.caps.KeyboardEffects {
		return State{}, ErrUnsupported
	}
	if err := value.Validate(); err != nil {
		return State{}, err
	}
	if err := c.fs.WriteFile(path.Join(keyboardBase, "four_zone_mode"), []byte(value.String())); err != nil {
		return State{}, err
	}
	c.invalidateSlow()
	return c.snapshot()
}

func (c *Controller) readText(file string) (string, error) {
	raw, err := c.fs.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (c *Controller) readBool(file string) (bool, error) {
	raw, err := c.readText(file)
	if err != nil {
		return false, err
	}
	switch raw {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s returned %q instead of 0 or 1", file, raw)
	}
}

func (c *Controller) readPowerSource() string {
	entries, err := c.fs.ReadDir("sys/class/power_supply")
	if err != nil {
		return "unknown"
	}
	foundMains := false
	for _, entry := range entries {
		base := path.Join("sys/class/power_supply", entry.Name())
		supplyType, _ := c.readText(path.Join(base, "type"))
		if supplyType != "Mains" && !strings.HasPrefix(strings.ToUpper(entry.Name()), "AC") {
			continue
		}
		foundMains = true
		online, err := c.readText(path.Join(base, "online"))
		if err == nil && online == "1" {
			return "ac"
		}
	}
	if foundMains {
		return "battery"
	}
	return "unknown"
}

func (c *Controller) readFanRPM() (*int, *int) {
	cpu := c.readOptionalInt(c.sensors.cpuFan)
	gpu := c.readOptionalInt(c.sensors.gpuFan)
	if c.sensorsWentStale(c.sensors.cpuFan, cpu == nil, c.sensors.gpuFan, gpu == nil) {
		cpu = c.readOptionalInt(c.sensors.cpuFan)
		gpu = c.readOptionalInt(c.sensors.gpuFan)
	}
	return cpu, gpu
}

func (c *Controller) readTemperatures() (*float64, *float64) {
	cpu := c.readOptionalTemperature(c.sensors.cpuTemp)
	gpu := c.readOptionalTemperature(c.sensors.gpuTemp)
	if c.sensorsWentStale(c.sensors.cpuTemp, cpu == nil, c.sensors.gpuTemp, gpu == nil) {
		cpu = c.readOptionalTemperature(c.sensors.cpuTemp)
		gpu = c.readOptionalTemperature(c.sensors.gpuTemp)
	}
	return cpu, gpu
}

// sensorsWentStale re-resolves the cache when a path that existed at resolve
// time stops reading. hwmon indices are not stable: swapping the GPU driver
// from nouveau to NVIDIA proprietary renumbered acer from hwmon4 to hwmon3 on
// the reference machine. Reports whether a re-resolve happened.
func (c *Controller) sensorsWentStale(firstPath string, firstFailed bool, secondPath string, secondFailed bool) bool {
	if (firstPath == "" || !firstFailed) && (secondPath == "" || !secondFailed) {
		return false
	}
	c.sensors = c.resolveSensors()
	return true
}

// resolveSensors locates every hwmon input once, so a telemetry tick reads four
// files instead of walking /sys/class/hwmon and every name attribute in it.
//
// Channels are matched by *_label where the driver publishes them, falling back
// to the documented channel order otherwise. Only paths that actually exist are
// recorded, so an empty field means "this machine has no such sensor".
//
// The GPU is the awkward case. The NVIDIA proprietary driver publishes no hwmon
// node at all — nvidia-smi is its only interface — so on an NVIDIA machine the
// acer EC channel is the only in-kernel GPU reading available. Measured against
// nvidia-smi on a PHN16-71 the two agree to within 1°C, so it is a sound
// fallback rather than a guess. A dedicated GPU sensor still wins when present.
func (c *Controller) resolveSensors() sensorPaths {
	var resolved sensorPaths
	var acerCPUTemp, acerGPUTemp string
	for _, dir := range c.hwmonDirectories() {
		name, err := c.readText(path.Join(dir, "name"))
		if err != nil {
			continue
		}
		switch strings.ToLower(name) {
		case "coretemp":
			resolved.cpuTemp = c.firstExisting(
				c.inputByLabel(dir, "temp", "Package id 0"),
				path.Join(dir, "temp1_input"))
		case "nvidia", "amdgpu":
			resolved.gpuTemp = c.firstExisting(path.Join(dir, "temp1_input"))
		case "acer":
			acerCPUTemp = c.firstExisting(
				c.inputByLabel(dir, "temp", "CPU"),
				path.Join(dir, "temp1_input"))
			acerGPUTemp = c.firstExisting(
				c.inputByLabel(dir, "temp", "GPU"),
				path.Join(dir, "temp2_input"))
			resolved.cpuFan = c.firstExisting(
				c.inputByLabel(dir, "fan", "CPU Fan"),
				path.Join(dir, "fan1_input"))
			resolved.gpuFan = c.firstExisting(
				c.inputByLabel(dir, "fan", "GPU Fan"),
				path.Join(dir, "fan2_input"))
		}
	}
	if resolved.cpuTemp == "" {
		resolved.cpuTemp = acerCPUTemp
	}
	if resolved.gpuTemp == "" {
		resolved.gpuTemp = acerGPUTemp
	}
	return resolved
}

func (c *Controller) firstExisting(candidates ...string) string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, err := c.fs.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// inputByLabel resolves the <prefix>N_input file whose sibling <prefix>N_label
// reads target, so callers never have to assume a channel number. Returns an
// empty path when the device publishes no labels or none match.
func (c *Controller) inputByLabel(dir, prefix, target string) string {
	entries, err := c.fs.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, "_label") {
			continue
		}
		label, err := c.readText(path.Join(dir, name))
		if err != nil || !strings.EqualFold(label, target) {
			continue
		}
		return path.Join(dir, strings.TrimSuffix(name, "_label")+"_input")
	}
	return ""
}

func (c *Controller) hwmonDirectories() []string {
	entries, err := c.fs.ReadDir("sys/class/hwmon")
	if err != nil {
		return nil
	}
	result := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
			result = append(result, path.Join("sys/class/hwmon", entry.Name()))
		}
	}
	sort.Strings(result)
	return result
}

func (c *Controller) readOptionalInt(file string) *int {
	if file == "" {
		return nil
	}
	raw, err := c.readText(file)
	if err != nil {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil
	}
	return &value
}

func (c *Controller) readOptionalTemperature(file string) *float64 {
	value := c.readOptionalInt(file)
	if value == nil {
		return nil
	}
	result := float64(*value) / 1000
	return &result
}

var profileOptions = []ProfileOption{
	{Value: "low-power", Label: "Eco", Description: "Prioritizes energy efficiency and battery life."},
	{Value: "quiet", Label: "Silent", Description: "Minimizes noise and prioritizes low power."},
	{Value: "balanced", Label: "Balanced", Description: "Balances performance and noise for everyday tasks."},
	{Value: "balanced-performance", Label: "Performance", Description: "Maximizes speed for demanding workloads."},
	{Value: "performance", Label: "Turbo", Description: "Unleashes peak power with the loudest cooling."},
}

func profileForValue(value string) ProfileOption {
	for _, option := range profileOptions {
		if option.Value == value {
			return option
		}
	}
	return ProfileOption{Value: value, Label: value}
}

func availableProfiles(choices []string, powerSource string) []ProfileOption {
	choiceSet := make(map[string]bool, len(choices))
	for _, choice := range choices {
		choiceSet[choice] = true
	}
	var permitted map[string]bool
	switch powerSource {
	case "battery":
		permitted = map[string]bool{"low-power": true, "balanced": true}
	case "ac":
		permitted = map[string]bool{
			"quiet": true, "balanced": true, "balanced-performance": true, "performance": true,
		}
	default:
		permitted = map[string]bool{"balanced": true}
	}
	result := make([]ProfileOption, 0, len(profileOptions))
	for _, option := range profileOptions {
		if choiceSet[option.Value] && permitted[option.Value] {
			result = append(result, option)
		}
	}
	return result
}
