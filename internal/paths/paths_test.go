package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStateDirOverrideWins(t *testing.T) {
	look := func(k string) (string, bool) {
		if k == StateDirEnv {
			return "/tmp/lease-here", true
		}
		return "", false
	}
	if got := StateDir(look); got != "/tmp/lease-here" {
		t.Fatalf("StateDir = %q, want the override", got)
	}
}

func TestStateDirUsesXDGStateHome(t *testing.T) {
	look := func(k string) (string, bool) {
		if k == "XDG_STATE_HOME" {
			return "/state", true
		}
		return "", false
	}
	if got := StateDir(look); got != filepath.Join("/state", "lease") {
		t.Fatalf("StateDir = %q, want /state/lease", got)
	}
}

func TestStateDirFallsBackToHome(t *testing.T) {
	look := func(string) (string, bool) { return "", false }
	got := StateDir(look)
	if !strings.Contains(got, "lease") {
		t.Fatalf("StateDir = %q, want a lease directory under the user state dir", got)
	}
}

func TestConfigFileOverrideWins(t *testing.T) {
	look := func(k string) (string, bool) {
		if k == ConfigEnv {
			return "/tmp/lease.toml", true
		}
		return "", false
	}
	if got := ConfigFile(look); got != "/tmp/lease.toml" {
		t.Fatalf("ConfigFile = %q, want the override", got)
	}
}

func TestConfigFileUnderXDGConfigHome(t *testing.T) {
	look := func(k string) (string, bool) {
		if k == "XDG_CONFIG_HOME" {
			return "/conf", true
		}
		return "", false
	}
	if got := ConfigFile(look); got != filepath.Join("/conf", "lease", "config.toml") {
		t.Fatalf("ConfigFile = %q, want /conf/lease/config.toml", got)
	}
}
