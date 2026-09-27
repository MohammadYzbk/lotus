package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// settings is the small amount of state that must survive a restart. Reopening
// wherever you left off is the difference between a tool and a demo.
//
// It lives in the config directory, not the cache: losing the last-open project
// to a cache purge would be a bad surprise. Engine choice, theme, and keymap
// join it in later phases.
type settings struct {
	// LastProject is an absolute directory path.
	LastProject string `json:"lastProject,omitempty"`
	// RootFile is the compile entry point chosen for LastProject, relative to it.
	// Detection is only a guess, so an explicit choice has to be remembered.
	RootFile string `json:"rootFile,omitempty"`
	// OpenFile is the file that was being edited, relative to LastProject.
	OpenFile string `json:"openFile,omitempty"`
}

// appDir is the app's own directory under the user's config location
// (~/Library/Application Support/lotus on macOS).
func appDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}
	dir := filepath.Join(base, "lotus")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create app dir: %w", err)
	}
	return dir, nil
}

const settingsFileName = "settings.json"

// loadSettings never fails the caller: a missing or corrupt settings file means
// "no preferences yet", which is a perfectly good state to start in.
func loadSettings(path string) settings {
	data, err := os.ReadFile(path)
	if err != nil {
		return settings{}
	}
	var s settings
	if err := json.Unmarshal(data, &s); err != nil {
		return settings{}
	}
	return s
}

func saveSettings(path string, s settings) error {
	if path == "" {
		return fmt.Errorf("no settings path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	// Write and rename, so an interrupted save cannot leave a truncated file
	// that would silently reset the writer's preferences.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}
