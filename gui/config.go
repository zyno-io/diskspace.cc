// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Remembered launch choice values.
const (
	choiceAdmin = "admin"
	choiceUser  = "user"
)

type guiConfig struct {
	Choice string `json:"choice,omitempty"` // "", admin, user
}

// configPath is os.UserConfigDir()/diskspace.cc/gui.json.
func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "diskspace.cc", "gui.json"), nil
}

func loadConfig(path string) guiConfig {
	var c guiConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	if json.Unmarshal(b, &c) != nil {
		return guiConfig{}
	}
	if c.Choice != choiceAdmin && c.Choice != choiceUser {
		c.Choice = ""
	}
	return c
}

func saveConfig(path string, c guiConfig) error {
	if path == "" {
		return errors.New("no config directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
