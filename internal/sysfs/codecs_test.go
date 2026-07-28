package sysfs

import "testing"

func TestFanSettingCodec(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    FanSetting
		wantErr bool
	}{
		{name: "automatic", raw: "0,0\n", want: FanSetting{CPU: 0, GPU: 0}},
		{name: "manual", raw: "42,67", want: FanSetting{CPU: 42, GPU: 67}},
		{name: "max", raw: "100,100", want: FanSetting{CPU: 100, GPU: 100}},
		{name: "missing field", raw: "50", wantErr: true},
		{name: "too high", raw: "101,50", wantErr: true},
		{name: "negative", raw: "20,-1", wantErr: true},
		{name: "not numeric", raw: "fast,50", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseFanSetting(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseFanSetting(%q) succeeded, want error", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseFanSetting(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
			if got.String() != test.want.String() {
				t.Fatalf("round trip produced %q", got.String())
			}
		})
	}
}

func TestEffectSettingCodec(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    EffectSetting
		wantErr bool
	}{
		{
			name: "observed static",
			raw:  "0,5,100,1,0,174,199\n",
			want: EffectSetting{Mode: 0, Speed: 5, Brightness: 100, Direction: 1, Red: 0, Green: 174, Blue: 199},
		},
		{
			name: "upper bounds",
			raw:  "7,9,100,2,255,255,255",
			want: EffectSetting{Mode: 7, Speed: 9, Brightness: 100, Direction: 2, Red: 255, Green: 255, Blue: 255},
		},
		{
			// The EC reports direction 0 when the active mode has no direction.
			// Parsing must accept it; EffectSetting.Validate still rejects it on
			// the write path.
			name: "device-reported unset direction",
			raw:  "0,0,100,0,0,0,0",
			want: EffectSetting{Mode: 0, Speed: 0, Brightness: 100, Direction: 0, Red: 0, Green: 0, Blue: 0},
		},
		{name: "wrong arity", raw: "1,2,3", wantErr: true},
		{name: "invalid mode", raw: "8,5,50,1,1,2,3", wantErr: true},
		{name: "invalid direction", raw: "1,5,50,3,1,2,3", wantErr: true},
		{name: "invalid blue", raw: "1,5,50,1,1,2,256", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseEffectSetting(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseEffectSetting(%q) succeeded, want error", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEffectSetting(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("got %#v, want %#v", got, test.want)
			}
			if got.String() != test.want.String() {
				t.Fatalf("round trip produced %q", got.String())
			}
		})
	}
}

func TestZoneSettingCodec(t *testing.T) {
	got, err := ParseZoneSetting("00AEC7,00aec7,abcdef,123456,100\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "00aec7,00aec7,abcdef,123456,100" {
		t.Fatalf("unexpected canonical value %q", got.String())
	}
	for _, raw := range []string{
		"00aec7,00aec7,00aec7,00aec7",
		"#0aec7,00aec7,00aec7,00aec7,50",
		"zzzzzz,00aec7,00aec7,00aec7,50",
		"00aec7,00aec7,00aec7,00aec7,101",
	} {
		if _, err := ParseZoneSetting(raw); err == nil {
			t.Errorf("ParseZoneSetting(%q) succeeded, want error", raw)
		}
	}
}
