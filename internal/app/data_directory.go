package app

import (
	"os"
	"path/filepath"
)

// Keep existing installations in place. Fresh installations use one stable
// machine-level registry regardless of the shell's current working directory.
func DefaultDataDirectory() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for _, parent := range []string{filepath.Dir(executable), cwd} {
		// A portable project's database must never become the global registry.
		if _, err := os.Lstat(filepath.Join(parent, manifestName)); err == nil {
			continue
		}
		candidate := filepath.Join(parent, ".tandem")
		if info, err := os.Stat(filepath.Join(candidate, "tandem.sqlite")); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "Tandem"), nil
}
