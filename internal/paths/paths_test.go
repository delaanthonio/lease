package paths

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSharedDirectoryGroupInheritance(t *testing.T) {
	if dir := os.Getenv("LEASE_TEST_SHARED_DIR"); dir != "" {
		f, err := OpenFile(filepath.Join(dir, "participant"))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	previous := Shared
	Shared = true
	t.Cleanup(func() { Shared = previous })
	root := t.TempDir()
	if err := os.Chmod(root, 0770|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "locks", "resource")
	if err := MkdirAll(dir); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(dir), dir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSetgid == 0 {
			t.Fatalf("%s does not inherit its group", path)
		}
	}
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range groups {
		if group == os.Getgid() {
			continue
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestSharedDirectoryGroupInheritance$")
		cmd.Env = append(os.Environ(), "LEASE_TEST_SHARED_DIR="+dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(group), NoSetGroups: true}}
		if out, err := cmd.CombinedOutput(); err != nil {
			if err == syscall.EPERM || strings.Contains(err.Error(), "operation not permitted") {
				t.Skip("cannot start a participant with a different primary group")
			}
			t.Fatalf("second participant: %v\n%s", err, out)
		}
		return
	}
	t.Skip("no supplementary group available for a second participant")
}

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
