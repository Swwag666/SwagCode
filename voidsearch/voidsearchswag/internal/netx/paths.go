package netx

import (
	"os"
	"path/filepath"
	"runtime"
)

func DefaultDataDir() string {
	if v := os.Getenv("VOIDSEARCH_DATA_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".voidsearchswag")
}

func VendorDir() string { return filepath.Join(DefaultDataDir(), "vendor", "tor") }

func DefaultTorDataDir() string { return filepath.Join(DefaultDataDir(), "tor-data") }

func DefaultStatePath() string { return filepath.Join(DefaultDataDir(), "proxy-pool.json") }

func DefaultDBPath() string { return filepath.Join(DefaultDataDir(), "voidsearchswag.db") }

func TorBinaryName() string {
	if runtime.GOOS == "windows" {
		return "tor.exe"
	}
	return "tor"
}
