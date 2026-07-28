package sysfs

import (
	"fmt"
	"strconv"
	"strings"
)

type FanSetting struct {
	CPU int `json:"cpu"`
	GPU int `json:"gpu"`
}

func ParseFanSetting(raw string) (FanSetting, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 2 {
		return FanSetting{}, fmt.Errorf("fan speed must contain cpu,gpu")
	}
	cpu, err := parseRange(parts[0], 0, 100, "CPU fan")
	if err != nil {
		return FanSetting{}, err
	}
	gpu, err := parseRange(parts[1], 0, 100, "GPU fan")
	if err != nil {
		return FanSetting{}, err
	}
	return FanSetting{CPU: cpu, GPU: gpu}, nil
}

func (f FanSetting) Validate() error {
	if f.CPU < 0 || f.CPU > 100 {
		return fmt.Errorf("CPU fan must be between 0 and 100")
	}
	if f.GPU < 0 || f.GPU > 100 {
		return fmt.Errorf("GPU fan must be between 0 and 100")
	}
	return nil
}

func (f FanSetting) String() string {
	return fmt.Sprintf("%d,%d", f.CPU, f.GPU)
}

type EffectSetting struct {
	Mode       int `json:"mode"`
	Speed      int `json:"speed"`
	Brightness int `json:"brightness"`
	Direction  int `json:"direction"`
	Red        int `json:"red"`
	Green      int `json:"green"`
	Blue       int `json:"blue"`
}

func ParseEffectSetting(raw string) (EffectSetting, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 7 {
		return EffectSetting{}, fmt.Errorf("keyboard effect must contain mode,speed,brightness,direction,red,green,blue")
	}
	limits := [][3]any{
		{"mode", 0, 7},
		{"speed", 0, 9},
		{"brightness", 0, 100},
		// The EC reports direction 0 for modes where direction does not apply,
		// so the read side has to tolerate it. Writes still require 1 or 2 —
		// see EffectSetting.Validate.
		{"direction", 0, 2},
		{"red", 0, 255},
		{"green", 0, 255},
		{"blue", 0, 255},
	}
	values := make([]int, len(parts))
	for i, part := range parts {
		value, err := parseRange(part, limits[i][1].(int), limits[i][2].(int), limits[i][0].(string))
		if err != nil {
			return EffectSetting{}, err
		}
		values[i] = value
	}
	return EffectSetting{
		Mode: values[0], Speed: values[1], Brightness: values[2],
		Direction: values[3], Red: values[4], Green: values[5], Blue: values[6],
	}, nil
}

func (e EffectSetting) Validate() error {
	if e.Mode < 0 || e.Mode > 7 {
		return fmt.Errorf("mode must be between 0 and 7")
	}
	if e.Speed < 0 || e.Speed > 9 {
		return fmt.Errorf("speed must be between 0 and 9")
	}
	if e.Brightness < 0 || e.Brightness > 100 {
		return fmt.Errorf("brightness must be between 0 and 100")
	}
	if e.Direction < 1 || e.Direction > 2 {
		return fmt.Errorf("direction must be 1 or 2")
	}
	if e.Red < 0 || e.Red > 255 || e.Green < 0 || e.Green > 255 || e.Blue < 0 || e.Blue > 255 {
		return fmt.Errorf("RGB channels must be between 0 and 255")
	}
	return nil
}

func (e EffectSetting) String() string {
	return fmt.Sprintf("%d,%d,%d,%d,%d,%d,%d",
		e.Mode, e.Speed, e.Brightness, e.Direction, e.Red, e.Green, e.Blue)
}

type ZoneSetting struct {
	Zone1      string `json:"zone1"`
	Zone2      string `json:"zone2"`
	Zone3      string `json:"zone3"`
	Zone4      string `json:"zone4"`
	Brightness int    `json:"brightness"`
}

func ParseZoneSetting(raw string) (ZoneSetting, error) {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	if len(parts) != 5 {
		return ZoneSetting{}, fmt.Errorf("per-zone value must contain zone1,zone2,zone3,zone4,brightness")
	}
	z := ZoneSetting{
		Zone1: strings.ToLower(parts[0]), Zone2: strings.ToLower(parts[1]),
		Zone3: strings.ToLower(parts[2]), Zone4: strings.ToLower(parts[3]),
	}
	for _, value := range []string{z.Zone1, z.Zone2, z.Zone3, z.Zone4} {
		if !isHexColor(value) {
			return ZoneSetting{}, fmt.Errorf("zone colours must be six hexadecimal characters without #")
		}
	}
	brightness, err := parseRange(parts[4], 0, 100, "brightness")
	if err != nil {
		return ZoneSetting{}, err
	}
	z.Brightness = brightness
	return z, nil
}

func (z ZoneSetting) Validate() error {
	for _, value := range []string{z.Zone1, z.Zone2, z.Zone3, z.Zone4} {
		if !isHexColor(value) {
			return fmt.Errorf("zone colours must be six hexadecimal characters without #")
		}
	}
	if z.Brightness < 0 || z.Brightness > 100 {
		return fmt.Errorf("brightness must be between 0 and 100")
	}
	return nil
}

func (z ZoneSetting) String() string {
	return fmt.Sprintf("%s,%s,%s,%s,%d",
		strings.ToLower(z.Zone1), strings.ToLower(z.Zone2),
		strings.ToLower(z.Zone3), strings.ToLower(z.Zone4), z.Brightness)
}

func parseRange(raw string, minValue, maxValue int, label string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", label)
	}
	if value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s must be between %d and %d", label, minValue, maxValue)
	}
	return value, nil
}

func isHexColor(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
