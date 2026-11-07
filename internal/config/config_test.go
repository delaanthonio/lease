package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    map[string]int
		wantErr bool
	}{
		{
			name:    "empty file is defaults",
			content: "",
			want:    map[string]int{},
		},
		{
			name: "slot counts are read",
			content: `
[resources.ios-simulator]
slots = 2

[resources.dev-port]
slots = 4
`,
			want: map[string]int{"ios-simulator": 2, "dev-port": 4},
		},
		{
			name:    "zero slots falls back to default at use time",
			content: "[resources.x]\nslots = 0\n",
			want:    map[string]int{"x": 0},
		},
		{
			name:    "negative slots are an error",
			content: "[resources.x]\nslots = -1\n",
			wantErr: true,
		},
		{
			name:    "malformed toml is an error",
			content: "[resources\n",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			cfg, err := Load(path)
			if c.wantErr {
				if err == nil {
					t.Fatalf("Load succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Path != path {
				t.Fatalf("Path = %q, want %q", cfg.Path, path)
			}
			for name, slots := range c.want {
				if got := cfg.Resources[name].Slots; got != slots {
					t.Errorf("resource %q slots = %d, want %d", name, got, slots)
				}
			}
		})
	}
}

func TestMissingFileIsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("missing config must not error: %v", err)
	}
	if cfg.Path != "" {
		t.Fatalf("Path = %q, want empty", cfg.Path)
	}
	if cfg.Slots("anything") != DefaultSlots {
		t.Fatalf("unknown resource must default to %d slots", DefaultSlots)
	}
}

func TestSlotsDefaults(t *testing.T) {
	cfg := &Config{Resources: map[string]Resource{"two": {Slots: 2}}}
	cases := []struct {
		resource string
		want     int
	}{
		{"two", 2},
		{"unknown", DefaultSlots},
		{"zero", DefaultSlots}, // declared with 0 slots behaves as unset
	}
	for _, c := range cases {
		if got := cfg.Slots(c.resource); got != c.want {
			t.Errorf("Slots(%q) = %d, want %d", c.resource, got, c.want)
		}
	}
}
