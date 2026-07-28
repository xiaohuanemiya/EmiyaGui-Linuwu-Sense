package sysfs

import (
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

type mapFS struct {
	files fstest.MapFS
}

func (m *mapFS) ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(m.files, name)
}

func (m *mapFS) WriteFile(name string, data []byte) error {
	file, ok := m.files[name]
	if !ok {
		return fs.ErrNotExist
	}
	file.Data = append([]byte(nil), data...)
	m.files[name] = file
	return nil
}

func (m *mapFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return fs.ReadDir(m.files, name)
}

func (m *mapFS) Stat(name string) (fs.FileInfo, error) {
	return fs.Stat(m.files, name)
}

func fixtureFS() *mapFS {
	file := func(value string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(value), Mode: 0o660}
	}
	return &mapFS{files: fstest.MapFS{
		path.Join(predatorBase, "fan_speed"):            file("0,0\n"),
		path.Join(predatorBase, "battery_limiter"):      file("0\n"),
		path.Join(predatorBase, "battery_calibration"):  file("0\n"),
		path.Join(predatorBase, "backlight_timeout"):    file("1\n"),
		path.Join(predatorBase, "lcd_override"):         file("0\n"),
		path.Join(predatorBase, "usb_charging"):         file("30\n"),
		path.Join(predatorBase, "boot_animation_sound"): file("1\n"),
		path.Join(predatorBase, "version"):              file("1.0.0-test\n"),
		path.Join(keyboardBase, "per_zone_mode"):        file("00aec7,00aec7,00aec7,00aec7,100\n"),
		path.Join(keyboardBase, "four_zone_mode"):       file("0,5,100,1,0,174,199\n"),
		profilePath:                          file("balanced\n"),
		choicesPath:                          file("low-power quiet balanced balanced-performance performance\n"),
		"sys/class/power_supply/ACAD/type":   file("Mains\n"),
		"sys/class/power_supply/ACAD/online": file("1\n"),
		"sys/class/hwmon/hwmon4/name":        file("acer\n"),
		"sys/class/hwmon/hwmon4/fan1_input":  file("1786\n"),
		"sys/class/hwmon/hwmon4/fan2_input":  file("1760\n"),
		"sys/class/hwmon/hwmon6/name":        file("coretemp\n"),
		"sys/class/hwmon/hwmon6/temp1_label": file("Package id 0\n"),
		"sys/class/hwmon/hwmon6/temp1_input": file("48000\n"),
	}}
}

func TestSnapshotAndPowerFilteredProfiles(t *testing.T) {
	controller := NewController(fixtureFS())
	state, err := controller.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if state.Profile.Label != "Balanced" || state.PowerSource != "ac" {
		t.Fatalf("unexpected state: %#v", state)
	}
	got := make([]string, 0, len(state.Profile.Available))
	for _, option := range state.Profile.Available {
		got = append(got, option.Value)
	}
	if strings.Join(got, ",") != "quiet,balanced,balanced-performance,performance" {
		t.Fatalf("unexpected AC profiles: %v", got)
	}
	if state.Temperatures.CPU == nil || *state.Temperatures.CPU != 48 {
		t.Fatalf("unexpected CPU temperature: %#v", state.Temperatures.CPU)
	}
	if state.Temperatures.GPU != nil {
		t.Fatalf("GPU temperature must be absent when no identifiable sensor exists")
	}
}

func TestFanSafetyInterlocks(t *testing.T) {
	files := fixtureFS()
	controller := NewController(files)
	files.files[profilePath].Data = []byte("quiet\n")
	if _, err := controller.SetFans(FanSetting{CPU: 50, GPU: 50}, false); err == nil || !strings.Contains(err.Error(), ErrConflict.Error()) {
		t.Fatalf("manual fans in quiet mode: got %v, want conflict", err)
	}
	if _, err := controller.SetFans(FanSetting{CPU: 0, GPU: 0}, false); err != nil {
		t.Fatalf("automatic fans should remain available in quiet mode: %v", err)
	}

	files.files[profilePath].Data = []byte("performance\n")
	if _, err := controller.SetFans(FanSetting{CPU: 19, GPU: 50}, false); err != ErrConfirmation {
		t.Fatalf("low manual fan under Turbo: got %v, want confirmation", err)
	}
	if _, err := controller.SetFans(FanSetting{CPU: 19, GPU: 50}, true); err != nil {
		t.Fatalf("confirmed low fan setting failed: %v", err)
	}
}

func mapFile(value string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(value), Mode: 0o660}
}

// dropCoretemp removes the fixture's dedicated CPU sensor so the acer EC
// channels become the only source left.
func dropCoretemp(files *mapFS) {
	for name := range files.files {
		if strings.HasPrefix(name, "sys/class/hwmon/hwmon6/") {
			delete(files.files, name)
		}
	}
}

func TestAcerLabelsBeatChannelOrder(t *testing.T) {
	files := fixtureFS()
	dropCoretemp(files)
	// Labels reversed against the conventional channel order on purpose: a
	// reader that assumes temp1 is the CPU reports these two swapped.
	files.files["sys/class/hwmon/hwmon4/temp1_label"] = mapFile("GPU\n")
	files.files["sys/class/hwmon/hwmon4/temp1_input"] = mapFile("61000\n")
	files.files["sys/class/hwmon/hwmon4/temp2_label"] = mapFile("CPU\n")
	files.files["sys/class/hwmon/hwmon4/temp2_input"] = mapFile("52000\n")
	files.files["sys/class/hwmon/hwmon4/fan1_label"] = mapFile("GPU Fan\n")
	files.files["sys/class/hwmon/hwmon4/fan2_label"] = mapFile("CPU Fan\n")

	controller := NewController(files)
	cpu, gpu := controller.readTemperatures()
	if cpu == nil || *cpu != 52 {
		t.Fatalf("CPU temperature: got %#v, want 52", cpu)
	}
	if gpu == nil || *gpu != 61 {
		t.Fatalf("GPU temperature: got %#v, want 61", gpu)
	}

	// Fixture RPMs are fan1=1786, fan2=1760.
	cpuRPM, gpuRPM := controller.readFanRPM()
	if cpuRPM == nil || *cpuRPM != 1760 {
		t.Fatalf("CPU fan: got %#v, want 1760", cpuRPM)
	}
	if gpuRPM == nil || *gpuRPM != 1786 {
		t.Fatalf("GPU fan: got %#v, want 1786", gpuRPM)
	}
}

func TestUnlabelledAcerFallsBackToChannelOrder(t *testing.T) {
	files := fixtureFS()
	dropCoretemp(files)
	files.files["sys/class/hwmon/hwmon4/temp1_input"] = mapFile("44000\n")
	files.files["sys/class/hwmon/hwmon4/temp2_input"] = mapFile("39000\n")

	controller := NewController(files)
	cpu, gpu := controller.readTemperatures()
	if cpu == nil || *cpu != 44 {
		t.Fatalf("CPU temperature: got %#v, want 44", cpu)
	}
	if gpu == nil || *gpu != 39 {
		t.Fatalf("GPU temperature: got %#v, want 39", gpu)
	}
	if !controller.caps.GPUTemperature {
		t.Fatal("acer GPU channel should satisfy the GPU temperature capability")
	}

	cpuRPM, gpuRPM := controller.readFanRPM()
	if cpuRPM == nil || *cpuRPM != 1786 {
		t.Fatalf("CPU fan: got %#v, want 1786", cpuRPM)
	}
	if gpuRPM == nil || *gpuRPM != 1760 {
		t.Fatalf("GPU fan: got %#v, want 1760", gpuRPM)
	}
}

func TestDedicatedGPUSensorBeatsAcerFallback(t *testing.T) {
	files := fixtureFS()
	files.files["sys/class/hwmon/hwmon4/temp2_label"] = mapFile("GPU\n")
	files.files["sys/class/hwmon/hwmon4/temp2_input"] = mapFile("39000\n")
	files.files["sys/class/hwmon/hwmon7/name"] = mapFile("amdgpu\n")
	files.files["sys/class/hwmon/hwmon7/temp1_input"] = mapFile("71000\n")

	_, gpu := NewController(files).readTemperatures()
	if gpu == nil || *gpu != 71 {
		t.Fatalf("dedicated GPU sensor should win: got %#v, want 71", gpu)
	}
}

// countingFS records reads so a test can prove the slow-value cache actually
// removes EC traffic rather than merely appearing to.
type countingFS struct {
	*mapFS
	reads map[string]int
}

func (c *countingFS) ReadFile(name string) ([]byte, error) {
	c.reads[name]++
	return c.mapFS.ReadFile(name)
}

// failingFS makes a path exist but fail on read, matching a sysfs attribute
// that probes fine yet whose ACPI call returns an error.
type failingFS struct {
	*mapFS
	fail map[string]bool
}

func (f *failingFS) ReadFile(name string) ([]byte, error) {
	if f.fail[name] {
		return nil, fs.ErrInvalid
	}
	return f.mapFS.ReadFile(name)
}

func TestSlowSettingsAreCachedBetweenSnapshots(t *testing.T) {
	counting := &countingFS{mapFS: fixtureFS(), reads: map[string]int{}}
	controller := NewController(counting)
	limiter := path.Join(predatorBase, "battery_limiter")
	for i := 0; i < 5; i++ {
		if _, err := controller.Snapshot(); err != nil {
			t.Fatal(err)
		}
	}
	if got := counting.reads[limiter]; got != 1 {
		t.Fatalf("battery_limiter read %d times across 5 snapshots, want 1", got)
	}
	// Temperatures must still be read every time.
	if got := counting.reads["sys/class/hwmon/hwmon6/temp1_input"]; got < 5 {
		t.Fatalf("CPU temperature read %d times across 5 snapshots, want at least 5", got)
	}
}

func TestWriteInvalidatesSlowCache(t *testing.T) {
	counting := &countingFS{mapFS: fixtureFS(), reads: map[string]int{}}
	controller := NewController(counting)
	if _, err := controller.Snapshot(); err != nil {
		t.Fatal(err)
	}
	limiter := path.Join(predatorBase, "battery_limiter")
	before := counting.reads[limiter]
	state, err := controller.SetBoolean("battery-limiter", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if counting.reads[limiter] <= before {
		t.Fatal("a write must force the settings cache to refresh")
	}
	if !state.Settings.BatteryLimiter {
		t.Fatal("read-back did not reflect the write")
	}
}

func TestSnapshotDegradesInsteadOfFailing(t *testing.T) {
	failing := &failingFS{mapFS: fixtureFS(), fail: map[string]bool{}}
	controller := NewController(failing)
	// Reproduces the observed production failure: platform_profile exists at
	// probe time but its ACPI read returns "operation not supported".
	failing.fail[profilePath] = true

	state, err := controller.Snapshot()
	if err != nil {
		t.Fatalf("one failing attribute must not fail the whole snapshot: %v", err)
	}
	if !slices.Contains(state.Degraded, "thermalProfile") {
		t.Fatalf("degraded should name thermalProfile, got %v", state.Degraded)
	}
	if state.Temperatures.CPU == nil || *state.Temperatures.CPU != 48 {
		t.Fatalf("CPU temperature lost to an unrelated failure: %#v", state.Temperatures.CPU)
	}
	if state.Fans.CPURPM == nil || *state.Fans.CPURPM != 1786 {
		t.Fatalf("fan RPM lost to an unrelated failure: %#v", state.Fans.CPURPM)
	}
}

// The PHN16-71 reports "0,0,100,0,0,0,0" at rest. Rejecting direction 0 on read
// used to fail the whole snapshot; writes must still refuse it.
func TestEffectParsesDeviceReportedZeroDirection(t *testing.T) {
	effect, err := ParseEffectSetting("0,0,100,0,0,0,0\n")
	if err != nil {
		t.Fatalf("device-reported effect must parse: %v", err)
	}
	if effect.Direction != 0 {
		t.Fatalf("direction: got %d, want 0 preserved", effect.Direction)
	}
	if err := effect.Validate(); err == nil {
		t.Fatal("writing direction 0 back must still be rejected")
	}
}

func TestBatteryCalibrationRequiresConfirmation(t *testing.T) {
	controller := NewController(fixtureFS())
	if _, err := controller.SetBoolean("battery-calibration", true, false); err != ErrConfirmation {
		t.Fatalf("got %v, want confirmation", err)
	}
	state, err := controller.SetBoolean("battery-calibration", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Settings.BatteryCalibration {
		t.Fatal("read-back did not reflect enabled calibration")
	}
}
