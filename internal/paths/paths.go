// Package paths resolves the machine-wide directories lease keeps its
// state in. Everything lives outside any worktree so locks survive
// worktree churn and are shared by every caller on the machine.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// StateDirEnv is the environment variable that overrides the default
// state directory.
const StateDirEnv = "LEASE_STATE_DIR"

// ConfigEnv is the environment variable that overrides the default
// config-file location.
const ConfigEnv = "LEASE_CONFIG"

// StateDir returns the machine-wide directory for lock files, holder
// records, waiter tickets, and history. The override environment variable
// wins; otherwise the directory sits under the user state dir
// (XDG_STATE_HOME/lease or the platform default). lookEnv is injected for
// tests.
func StateDir(lookEnv func(string) (string, bool)) string {
	if v, ok := lookEnv("LEASE_DIR"); ok && v != "" {
		return v
	}
	if v, ok := lookEnv(StateDirEnv); ok && v != "" {
		return v
	}
	if xdg, ok := lookEnv("XDG_STATE_HOME"); ok && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "lease")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home directory: fall back to a writable temp location so the
		// tool still works in hermetic environments.
		return filepath.Join(os.TempDir(), "lease")
	}
	if isDarwin() {
		return filepath.Join(home, "Library", "Application Support", "lease")
	}
	return filepath.Join(home, ".local", "state", "lease")
}

// ConfigFile returns the config-file path to read: the override
// environment variable wins, otherwise the XDG config location.
func ConfigFile(lookEnv func(string) (string, bool)) string {
	if v, ok := lookEnv(ConfigEnv); ok && v != "" {
		return v
	}
	if xdg, ok := lookEnv("XDG_CONFIG_HOME"); ok && filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "lease", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if isDarwin() {
		return filepath.Join(home, "Library", "Application Support", "lease", "config.toml")
	}
	return filepath.Join(home, ".config", "lease", "config.toml")
}

func isDarwin() bool {
	return runtime.GOOS == "darwin"
}

var Shared bool

func DirMode() os.FileMode {
	if Shared {
		return 0770
	}
	return 0700
}
func FileMode() os.FileMode {
	if Shared {
		return 0660
	}
	return 0600
}

func MkdirAll(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, DirMode()); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	mode := DirMode()
	if Shared {
		mode |= os.ModeSetgid
	}
	return os.Chmod(path, mode)
}

func OpenFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, FileMode())
	if err == nil {
		if err := f.Chmod(FileMode()); err != nil {
			f.Close()
			return nil, err
		}
		return f, nil
	}
	if !os.IsExist(err) {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR, 0)
}
